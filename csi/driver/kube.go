package driver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// kube is the Kubernetes API, from inside a pod, with its service account. It uses only the standard library: see
// decision 40.
type kube struct {
	base      string
	hc        *http.Client
	saDir     string
	namespace string
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount/"

// inCluster returns the API as this pod's service account sees it.
func inCluster() (*kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not in a Kubernetes pod: KUBERNETES_SERVICE_HOST and _PORT aren't set")
	}
	ca, err := os.ReadFile(serviceAccountDir + "ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %sca.crt", serviceAccountDir)
	}
	ns, err := os.ReadFile(serviceAccountDir + "namespace")
	if err != nil {
		return nil, err
	}
	return &kube{
		base: "https://" + net.JoinHostPort(host, port),
		hc: &http.Client{Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
		saDir:     serviceAccountDir,
		namespace: strings.TrimSpace(string(ns)),
	}, nil
}

// kubeError is a status from the API that isn't success.
type kubeError struct {
	Code int
	Body string
}

func (e *kubeError) Error() string { return fmt.Sprintf("Kubernetes API: %d: %s", e.Code, e.Body) }

func isKubeStatus(err error, code int) bool {
	ke, ok := err.(*kubeError)
	return ok && ke.Code == code
}

// do sends a request, with body as JSON if it isn't nil, and decodes the answer into out if that isn't nil.
func (k *kube) do(ctx context.Context, method, path string, body, out any) error {
	// Read each time, as the kubelet rotates the token.
	token, err := os.ReadFile(k.saDir + "token")
	if err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &kubeError{resp.StatusCode, strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
