package main

import (
	"cmp"
	"slices"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

var (
	fyneApp *fyne.App
	window  fyne.Window

	// windowUpdates coalesces requests to redraw the window; it has room for
	// one pending request, so that senders never block.
	windowUpdates = make(chan struct{}, 1)
)

const (
	windowColumns   = 3
	windowMaxHeight = 700
)

// shownDevices is the number of devices in the last drawn window. The window
// is resized to fit its content only when it changes, so that a resize by the
// user is not undone by every state refresh. Only used by drawWindow.
var shownDevices = -1

// requestWindowUpdate asks for the device window to be redrawn. It never
// blocks, so it is safe to call with device locks held.
func requestWindowUpdate() {
	select {
	case windowUpdates <- struct{}{}:
	default:
	}
}

// initWindow creates the hidden device window. It must be called on the main
// goroutine before the Fyne event loop starts.
func initWindow(cfg *Config) fyne.App {
	a := app.NewWithID("lol.cicciogatto.tapotray")
	fyneApp = &a
	window = a.NewWindow("Tapo")
	// closing the window only hides it: the tray app keeps running
	window.SetCloseIntercept(window.Hide)
	window.SetContent(widget.NewLabel("Loading devices..."))
	go func() {
		for range windowUpdates {
			drawWindow(cfg)
		}
	}()
	return a
}

// showWindow shows the device window, or brings it to the front if visible.
func showWindow() {
	fyne.Do(func() {
		window.Show()
		window.RequestFocus()
	})
}

func quitWindow() {
	if fyneApp != nil {
		fyne.Do((*fyneApp).Quit)
	}
}

// drawWindow rebuilds the window content from the current devices: a button
// per device, highlighted when the device is on.
func drawWindow(cfg *Config) {
	deviceItemsMutex.Lock()
	devices := make([]*DeviceItem, len(deviceItems))
	copy(devices, deviceItems)
	deviceItemsMutex.Unlock()

	type entry struct {
		label string
		on    bool
		di    *DeviceItem
	}
	var plugEntries, bulbEntries []entry
	for _, di := range devices {
		di.mu.Lock()
		e := entry{label: di.label(), on: di.on, di: di}
		di.mu.Unlock()
		if di.kind == kindBulb {
			bulbEntries = append(bulbEntries, e)
		} else {
			plugEntries = append(plugEntries, e)
		}
	}

	buttons := func(entries []entry) []fyne.CanvasObject {
		slices.SortFunc(entries, func(a, b entry) int {
			return cmp.Or(
				cmp.Compare(strings.ToLower(a.label), strings.ToLower(b.label)),
				cmp.Compare(a.label, b.label),
			)
		})
		var ret []fyne.CanvasObject
		for _, e := range entries {
			btn := widget.NewButton(e.label, func() {
				go e.di.toggle(cfg)
			})
			if e.on {
				btn.Importance = widget.HighImportance
			}
			ret = append(ret, btn)
		}
		return ret
	}
	plugs, bulbs := buttons(plugEntries), buttons(bulbEntries)
	box := container.NewVBox()
	section := func(title string, buttons []fyne.CanvasObject) {
		if len(buttons) == 0 {
			return
		}
		box.Add(widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		box.Add(container.NewGridWithColumns(windowColumns, buttons...))
	}
	section("Plugs", plugs)
	section("Bulbs", bulbs)
	if len(devices) == 0 {
		box.Add(widget.NewLabel("No devices"))
	}
	fyne.Do(func() {
		window.SetContent(container.NewVScroll(box))
		if len(devices) != shownDevices {
			// the scroll container has no minimum height of its own, so the
			// size has to come from the content
			size := box.MinSize().AddWidthHeight(theme.Padding()*2, theme.Padding()*2)
			size.Height = min(size.Height, windowMaxHeight)
			window.Resize(size)
			shownDevices = len(devices)
		}
	})
}
