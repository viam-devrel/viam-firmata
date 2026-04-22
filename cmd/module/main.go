// Command viam-firmata is the module binary for the devrel:firmata registry
// module. It registers the devrel:firmata:board model via the side-effect
// import of the firmataboard package and hands control to module.ModularMain.
package main

import (
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	firmataboard "github.com/viam-devrel/viam-firmata"
)

func main() {
	module.ModularMain(resource.APIModel{API: board.API, Model: firmataboard.Model})
}
