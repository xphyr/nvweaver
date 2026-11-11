# nvweaver on a Synology

An NVMe/TCP target on a Synology, in a container, with Container Manager. DSM's kernel has no NVMe target, so nvweaver runs
SPDK, which serves NVMe/TCP itself. Its volumes are files on a btrfs volume, with instant snapshots and clones from reflink.
It needs no privileges: host networking, and one capability, `DAC_READ_SEARCH`, so that it can read files made in File
Station. See "A Synology's DSM, for SPDK first" in `docs/testing.md` for what's been tested, and how.

**Tested so far** on DSM 7.4.1, x86-64, in a Virtual DSM: the steps below, through Container Manager's screens, and with
`docker compose` from SSH, with a host connecting over NVMe/TCP, snapshots, clones, a snapshot group and a restart. **Not
yet** on a real Synology, or on an Arm model, which needs someone who has one: reports are welcome.

## What you need

- DSM 7.2 or later, with Container Manager, and a **btrfs** volume. Ext4 volumes have no reflink, and nvweaver won't adopt
  one as a pool.
- Hosts that connect with NVMe/TCP, such as Linux with `nvme-cli`, on a network that reaches the Synology.
- Memory: SPDK takes 1 GB at start, by default.

## Set it up

1. **Folders.** In File Station, in the `docker` shared folder, make `nvweaver`, and in it `config`, `data` and
   `data/pool`. The pool's files go in `data/pool`.
2. **Files.** Put `compose.yaml` from here in `nvweaver`, and copy `.env.example` to `.env` beside it, unchanged for now.
3. **Start it.** In Container Manager, Project, Create: name it `nvweaver`, with the path `/docker/nvweaver`, and use the
   existing `compose.yaml`. Or, over SSH, in that folder: `sudo docker compose up -d`. Its API is on loopback for now, so
   nothing can reach it yet.
4. **A token, and a user.** In Container Manager, open the `nvweaver` container's terminal, and run:
   ```sh
   openssl rand -hex 32         # the admin token: 64 hex characters
   nvweaver passwd admin        # asks for a password, and prints a line for the users file
   ```
   Put the token in `.env` as `NVWEAVER_API_TOKEN`, for the API and the `nvweaver` command line utility, and the line in
   `config/users`. Over SSH, the same works on the Synology itself, with `sudo docker exec -it nvweaver` before each.

   **Or make `config/users` beforehand with `htpasswd`,** on another machine, as DSM has none: it's in `httpd-tools` on
   Fedora and RHEL, `apache2-utils` on Debian and Ubuntu, and macOS has it. It must be bcrypt, `-B`; other formats are
   refused. Give it `-C 12`, the cost `nvweaver passwd` uses, as its default of 5 is far quicker to crack:
   ```sh
   htpasswd -B -C 12 -c users admin     # asks for the password; drop -c to add more users to the file
   ```
   Then copy `users` into `config` in File Station. Names are letters, digits, `.`, `_`, `@` and `-`, up to 64; passwords
   up to 72 bytes, bcrypt's limit.
5. **TLS.** Passwords aren't accepted over plain HTTP, so give it a certificate and key, as PEM files, `config/tls.crt` and
   `config/tls.key`. DSM can export one of its own, under Control Panel, Security, Certificate. Or, over SSH:
   ```sh
   cd /volume1/docker/nvweaver
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 825 -subj /CN=nvweaver \
     -addext "subjectAltName=IP:192.0.2.10" -keyout config/tls.key -out config/tls.crt
   chmod 600 config/tls.key
   ```
6. **Reach it from your network.** In `.env`, set `NVWEAVER_LISTEN` to the Synology's address, port 8420, such as
   `192.0.2.10:8420`, and the TLS and users lines, `NVWEAVER_TLS_CERT`, `NVWEAVER_TLS_KEY` and `NVWEAVER_USERS_FILE`, as in
   `.env.example`. Restart the project, then sign in at `https://<address>:8420/ui/`.
7. **A pool.** In the web interface, Pools, adopt a reflink pool with the path `/var/lib/nvweaver/pool`. Or:
   ```sh
   nvweaver --api https://192.0.2.10:8420 --ca tls.crt pool create syn --type reflink --path /var/lib/nvweaver/pool
   ```
8. **A port** on the address hosts will use, such as port 4420, then volumes, hosts and subsystems, as in the main README.

## Notes

- **Keep `.env`, `config/tls.key`, `config/users` and `config/tokens` private:** mode 0600, or in File Station, readable
  only by you. The container reads them anyway, with its one capability.
- **Snapshot groups** pause hosts' I/O for a moment, and need hosts with native NVMe multipath, Linux's default.
- **Stopping the project stops serving hosts.** Hosts using its volumes keep retrying until it's back, and carry on.
- **The CSI driver** works with it as with any nvweaver: give it a scoped token from `nvweaver token new`, in
  `config/tokens`, with `NVWEAVER_TOKENS_FILE=/etc/nvweaver/tokens`.
- **Why not the kernel target:** DSM's kernel, 4.4 on DSM 7.4, predates it, and has no LVM thin pools either.
