# sukad

Sukashi node daemon with Xray-family protocol and native mieru support.

> **Note:** This daemon must be paired with the [Sukashi](https://github.com/owo-space/sukashi) panel.

## Overview

`sukad` runs on each proxy node, pulls its configuration from the Sukashi
panel, serves user traffic over the Xray-family protocols and native mieru,
and reports traffic/online stats back to the panel.

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

## Star history

[![Stargazers over time](https://starchart.cc/owo-space/sukad.svg?variant=adaptive)](https://starchart.cc/owo-space/sukad)
