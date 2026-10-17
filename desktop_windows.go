//go:build desktop && windows

package main

import (
	"context"
	"errors"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

const desktopEnabled = true

func runDesktop(dbPath string, noAutoTuning bool) error {
	store, err := acquireStoreLock(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	app := NewApp()
	app.configFile = store.path
	if err := app.startup(context.Background()); err != nil {
		return err
	}
	app.applyTransferTuningOverride(noAutoTuning)
	// Run returns after the native window closes. Keep the store locked until
	// active requests and transfers have unwound and their journal is saved.
	err = wails.Run(desktopOptions(app))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(err, app.shutdown(ctx))
}

func desktopOptions(app *App) *options.App {
	return &options.App{
		Title: "Harbor", Width: 1280, Height: 820,
		MinWidth: 960, MinHeight: 640,
		AssetServer: &assetserver.Options{Handler: desktopHandler(app)},
		Windows:     &windows.Options{Theme: windows.SystemDefault},
		OnBeforeClose: func(context.Context) bool {
			app.beginShutdown()
			return false
		},
	}
}
