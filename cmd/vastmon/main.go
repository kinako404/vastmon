// Command vastmon is a self-hosted vast.ai GPU monitor and rental panel.
//
//	vastmon [-addr 0.0.0.0:8090] [-interval 2m] [-disk 96] [-db vastmon.db]
//	vastmon passwd     set the panel password
//	vastmon version    print the version
package main

import "github.com/kinako404/vastmon/internal/panel"

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	panel.Main(version)
}
