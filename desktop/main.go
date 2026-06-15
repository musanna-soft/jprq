package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

// version is reported to the server as the CLI version.
var version = "2.4"

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:  "jprq",
		Width:  520,
		Height: 680,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		Bind: []any{
			app,
		},
	})
	if err != nil {
		println("error:", err.Error())
	}
}
