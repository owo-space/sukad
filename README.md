# sukad

Sukashi node daemon with Xray-family protocols plus native Mieru and Snell support.

> **Note:** This daemon must be paired with the [Sukashi](https://github.com/owo-space/sukashi) panel.

## Overview

`sukad` runs on each proxy node, pulls its configuration from the Sukashi
panel, serves user traffic, and reports traffic/online stats back to the panel.

## Supported protocols

Xray-family (served via the embedded Xray core):

- Shadowsocks
- VLESS
- VMess
- Trojan
- Hysteria2
- TUIC
- AnyTLS

Native (standalone implementations):

- Mieru
- Snell

## Installation

### One-line install

```bash
wget -N https://raw.githubusercontent.com/owo-space/sukad/main/script/install.sh && bash install.sh
```

The installer sets up the `sukad` service. Manage it with:

```bash
sukad            # interactive management menu
sukad log        # view logs
systemctl status sukad
```

Node configuration lives in `/etc/sukad/config.json` and is populated with the
panel URL and server token during install.

## Build from source

Requires Go 1.26+.

```bash
GOEXPERIMENT=jsonv2 go build -v -o build_assets/sukad -trimpath \
  -ldflags "-X 'github.com/owo-space/sukad/cmd.version=$version' -s -w -buildid="
```

A `Dockerfile` is also provided for container builds.

## License

See [LICENSE](./LICENSE).
