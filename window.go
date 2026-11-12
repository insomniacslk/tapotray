package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/insomniacslk/tapo"
)

const windowMaxHeight = 700

var (
	fyneApp *fyne.App
	window  fyne.Window

	// windowUpdates coalesces requests to redraw the window; it has room for
	// one pending request, so that senders never block.
	windowUpdates = make(chan struct{}, 1)

	// selectedAddr is the address of the device whose details are shown in
	// the side panel; empty means the first device.
	selectedMutex sync.Mutex
	selectedAddr  string

	// shownShape describes the shape of the last drawn window. The window is
	// resized to fit its content only when it changes, so that a resize by
	// the user is not undone by every state refresh. Only used by drawWindow.
	shownShape string
	split      *container.Split
)

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

// windowEntry is a snapshot of a device, taken so that drawing does not hold
// any device lock.
type windowEntry struct {
	di     *DeviceItem
	label  string
	on     bool
	info   *tapo.DeviceInfo
	energy *tapo.EnergyUsage
	usage  *tapo.DeviceUsage
}

func boldLabel(text string) *widget.Label {
	return widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
}

// detailsPanel lists the details of the device, like `tapo info` does, plus
// its energy usage if it supports power monitoring.
func detailsPanel(e windowEntry) fyne.CanvasObject {
	box := container.NewVBox(widget.NewLabelWithStyle(e.label, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	table := func(title string, rows [][2]string) {
		box.Add(widget.NewSeparator())
		box.Add(boldLabel(title))
		var cells []fyne.CanvasObject
		for _, r := range rows {
			cells = append(cells, widget.NewLabel(r[0]), widget.NewLabel(r[1]))
		}
		box.Add(container.NewGridWithColumns(2, cells...))
	}
	if e.info == nil {
		box.Add(widget.NewLabel("No information yet"))
		return box
	}
	i := e.info
	state := "off"
	if i.DeviceON {
		state = "on"
	}
	rows := [][2]string{
		{"State", state},
		{"Model", i.Model},
		{"Type", i.Type},
		{"IP", i.IP},
		{"MAC", i.MAC},
		{"Device ID", i.DeviceID},
		{"Firmware", i.FWVersion},
		{"Hardware", i.HWVersion},
		{"Wi-Fi", fmt.Sprintf("%s (signal %d/3, %d dBm)", i.DecodedSSID, i.SignalLevel, i.RSSI)},
		{"On for", (time.Duration(i.OnTime) * time.Second).String()},
		{"Overheated", fmt.Sprint(i.OverHeated)},
	}
	if i.PowerProtectionStatus != "" {
		rows = append(rows, [2]string{"Power protection", i.PowerProtectionStatus})
	}
	table("Info", rows)
	if e.energy != nil && e.usage != nil {
		en, u := e.energy, e.usage
		minutes := func(m int) string { return (time.Duration(m) * time.Minute).String() }
		table("Energy usage", [][2]string{
			{"Power now", fmt.Sprintf("%.1f W", float64(en.CurrentPower)/1000.0)},
			{"Energy today", kwh(en.TodayEnergy)},
			{"Energy this month", kwh(en.MonthEnergy)},
			{"Runtime today", minutes(en.TodayRuntime)},
			{"Runtime this month", minutes(en.MonthRuntime)},
			{"Used today", kwh(u.PowerUsage.Today)},
			{"Used, last 7 days", kwh(u.PowerUsage.Past7)},
			{"Used, last 30 days", kwh(u.PowerUsage.Past30)},
			{"Saved today", kwh(u.SavedPower.Today)},
			{"Saved, last 7 days", kwh(u.SavedPower.Past7)},
			{"Saved, last 30 days", kwh(u.SavedPower.Past30)},
			{"On, last 7 days", minutes(u.TimeUsage.Past7)},
			{"On, last 30 days", minutes(u.TimeUsage.Past30)},
		})
	}
	return box
}

// drawWindow rebuilds the window content from the current devices: a column
// of toggle buttons, highlighted when the device is on, and a side panel with
// the details of the selected device.
func drawWindow(cfg *Config) {
	deviceItemsMutex.Lock()
	devices := make([]*DeviceItem, len(deviceItems))
	copy(devices, deviceItems)
	deviceItemsMutex.Unlock()

	var plugs, bulbs []windowEntry
	for _, di := range devices {
		di.mu.Lock()
		e := windowEntry{di: di, label: di.label(), on: di.on, info: di.info, energy: di.energy, usage: di.usage}
		di.mu.Unlock()
		if di.kind == kindBulb {
			bulbs = append(bulbs, e)
		} else {
			plugs = append(plugs, e)
		}
	}
	byLabel := func(a, b windowEntry) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.label), strings.ToLower(b.label)),
			cmp.Compare(a.label, b.label),
		)
	}
	slices.SortFunc(plugs, byLabel)
	slices.SortFunc(bulbs, byLabel)
	ordered := append(slices.Clone(plugs), bulbs...)

	selectedMutex.Lock()
	sel := selectedAddr
	selectedMutex.Unlock()
	selected := -1
	for i, e := range ordered {
		if e.di.addr == sel {
			selected = i
		}
	}
	if selected < 0 && len(ordered) > 0 {
		selected = 0
	}

	list := container.NewVBox()
	section := func(title string, entries []windowEntry) {
		if len(entries) == 0 {
			return
		}
		list.Add(boldLabel(title))
		for _, e := range entries {
			toggle := widget.NewButton(e.label, func() { go e.di.toggle(cfg) })
			if e.on {
				toggle.Importance = widget.HighImportance
			}
			details := widget.NewButtonWithIcon("", theme.InfoIcon(), func() {
				selectedMutex.Lock()
				selectedAddr = e.di.addr
				selectedMutex.Unlock()
				requestWindowUpdate()
			})
			if selected >= 0 && ordered[selected].di == e.di {
				details.Importance = widget.HighImportance
			}
			list.Add(container.NewBorder(nil, nil, nil, details, toggle))
		}
	}
	section("Plugs", plugs)
	section("Bulbs", bulbs)
	var panel fyne.CanvasObject
	if selected >= 0 {
		panel = detailsPanel(ordered[selected])
	} else {
		list.Add(widget.NewLabel("No devices"))
		panel = widget.NewLabel("")
	}
	hasInfo := selected >= 0 && ordered[selected].info != nil

	fyne.Do(func() {
		// the split is kept across redraws, so that the divider stays where
		// the user put it
		if split == nil {
			split = container.NewHSplit(widget.NewLabel(""), widget.NewLabel(""))
			window.SetContent(split)
		}
		split.Leading = container.NewVScroll(list)
		split.Trailing = container.NewVScroll(panel)
		split.Refresh()
		if shape := fmt.Sprintf("%d/%t", len(devices), hasInfo); shape != shownShape {
			// the scroll containers have no minimum height of their own, so
			// the size has to come from the content
			pad := theme.Padding() * 4
			size := fyne.NewSize(
				list.MinSize().Width+panel.MinSize().Width+pad,
				max(list.MinSize().Height, panel.MinSize().Height)+pad,
			)
			size.Height = min(size.Height, windowMaxHeight)
			split.SetOffset(float64(list.MinSize().Width / size.Width))
			window.Resize(size)
			shownShape = shape
		}
	})
}
