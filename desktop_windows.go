//go:build desktop && windows

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
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
	var pickerMu sync.Mutex
	var desktopContext context.Context
	pick := func(folder bool) ([]desktopUploadFile, error) {
		pickerMu.Lock()
		defer pickerMu.Unlock()
		if desktopContext == nil {
			return nil, errors.New("Desktop picker is not ready")
		}
		var paths []string
		var err error
		if folder {
			var path string
			path, err = wailsruntime.OpenDirectoryDialog(desktopContext, wailsruntime.OpenDialogOptions{Title: "Choose a folder to upload"})
			if path != "" {
				paths = []string{path}
			}
		} else {
			paths, err = wailsruntime.OpenMultipleFilesDialog(desktopContext, wailsruntime.OpenDialogOptions{Title: "Choose files to upload"})
		}
		if err != nil {
			return nil, err
		}
		return collectDesktopUploads(app.ctx, paths, folder)
	}
	return &options.App{
		Title: "Harbor", Width: 1280, Height: 820,
		MinWidth: 960, MinHeight: 640,
		AssetServer: &assetserver.Options{Handler: desktopHandler(app, pick)},
		OnStartup:   func(ctx context.Context) { pickerMu.Lock(); desktopContext = ctx; pickerMu.Unlock() },
		Windows:     &windows.Options{Theme: windows.SystemDefault},
		OnBeforeClose: func(context.Context) bool {
			app.beginShutdown()
			return false
		},
	}
}
