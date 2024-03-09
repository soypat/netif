# netif
[![go.dev reference](https://pkg.go.dev/badge/github.com/soypat/netif)](https://pkg.go.dev/github.com/soypat/netif)
[![Go Report Card](https://goreportcard.com/badge/github.com/soypat/netif)](https://goreportcard.com/report/github.com/soypat/netif)
[![codecov](https://codecov.io/gh/soypat/netif/branch/main/graph/badge.svg)](https://codecov.io/gh/soypat/netif)
[![Go](https://github.com/soypat/netif/actions/workflows/go.yml/badge.svg)](https://github.com/soypat/netif/actions/workflows/go.yml)
[![stability-frozen](https://img.shields.io/badge/stability-frozen-blue.svg)](https://github.com/emersion/stability-badges#frozen)
[![sourcegraph](https://sourcegraph.com/github.com/soypat/netif/-/badge.svg)](https://sourcegraph.com/github.com/soypat/netif?badge)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT) 


Networking interfaces. For now I'm adding a TAP interface I can understand

## Debugging with VSCode

To run with an ethernet interface you'll need to open VSCode as sudo with `--no-sandbox` option:
```sh
sudo code --user-data-dir ~/.config/Code --no-sandbox .
```

You may need to install the Go extension and Delve as sudo since you are opening VSCode as a different user.

Creating a launch.json maps directly to using VSCode normally. Here's an example:
<details>

```json5
{
    "version": "0.2.0",
    "configurations": [
        {
            "name": "Launch Package",
            "type": "go",
            "request": "launch",
            "mode": "auto",
            "program": "${workspaceFolder}/examples/mqtt/",
        }
    ]
}
```

</details>


