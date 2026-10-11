package main

import (
	"log"

	"github.com/Necho-dev/vartio-plugin-bitbrowser/bitbrowser"
	"github.com/Necho-dev/vartio-plugin-bitbrowser/sdk"
)

func main() {
	if err := sdk.Serve(bitbrowser.New()); err != nil {
		log.Fatal(err)
	}
}
