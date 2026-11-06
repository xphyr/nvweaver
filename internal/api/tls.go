package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

// expiryWarning is how long before a certificate expires that loading it logs a warning.
const expiryWarning = 30 * 24 * time.Hour

// NewTLSConfig returns the API's TLS settings, using a PEM certificate and key, or nil if neither is set. Setting only
// one is an error, as is a pair that can't be used: the API shouldn't quietly start without the protection that was asked for.
//
// The files are read again when they change, as restarting the service to renew a certificate would stop SPDK's data
// path. A change that can't be used keeps the current certificate, as a renewal replaces two files, one after the other.
func NewTLSConfig(certPath, keyPath string, log *slog.Logger) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("NVWEAVER_TLS_CERT and NVWEAVER_TLS_KEY must be set together")
	}
	r := &certReloader{certPath: certPath, keyPath: keyPath, log: log}
	stamp, err := r.stamp()
	if err != nil {
		return nil, fmt.Errorf("API TLS: %w", err)
	}
	if err := r.load(stamp); err != nil {
		return nil, fmt.Errorf("API TLS: %w", err)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return r.current(), nil
		},
	}, nil
}

// certReloader holds the certificate in use, checking at each handshake whether its files have changed.
type certReloader struct {
	certPath, keyPath string
	log               *slog.Logger

	mu     sync.Mutex
	cert   *tls.Certificate
	loaded string // The stamp of what's in cert.
	failed string // The stamp of what couldn't be used, so that it's only logged once.
}

// stamp identifies the current contents of both files. A replaced file has a new inode, even within one clock tick.
func (r *certReloader) stamp() (string, error) {
	var out string
	for _, path := range []string{r.certPath, r.keyPath} {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		var inode uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			inode = st.Ino
		}
		out += fmt.Sprintf("%d:%d:%d;", inode, info.Size(), info.ModTime().UnixNano())
	}
	return out, nil
}

func (r *certReloader) load(stamp string) error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.cert, r.loaded, r.failed = &cert, stamp, ""
	if leaf := cert.Leaf; leaf != nil {
		remaining := time.Until(leaf.NotAfter)
		switch {
		case remaining <= 0:
			r.log.Error("API TLS certificate has expired, and clients will refuse it", "not_after", leaf.NotAfter)
		case remaining < expiryWarning:
			r.log.Warn("API TLS certificate expires soon", "not_after", leaf.NotAfter)
		default:
			r.log.Info("API TLS certificate loaded", "not_after", leaf.NotAfter)
		}
	}
	return nil
}

func (r *certReloader) current() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamp, err := r.stamp()
	if err == nil && (stamp == r.loaded || stamp == r.failed) {
		return r.cert
	}
	if err == nil {
		err = r.load(stamp)
	} else {
		stamp = "unreadable: " + err.Error()
		if stamp == r.failed {
			return r.cert
		}
	}
	if err != nil {
		r.failed = stamp
		r.log.Error("keeping the current API TLS certificate, as its files can't be used", "error", err)
	}
	return r.cert
}
