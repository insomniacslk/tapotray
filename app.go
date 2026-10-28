package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/insomniacslk/tapo"
	"github.com/sirupsen/logrus"
)

// deviceSource is the method used to find devices beyond the configured ones.
type deviceSource int

const (
	sourceNone deviceSource = iota
	sourceDiscover
	sourceCloud
)

// deviceKind is the kind of Tapo device, which determines the menu section
// it is shown in.
type deviceKind int

const (
	kindPlug deviceKind = iota
	kindBulb
)

func (k deviceKind) String() string {
	if k == kindBulb {
		return "bulb"
	}
	return "plug"
}

// the menu can only grow by appending items, so each section pre-creates a
// fixed number of hidden menu items that found devices are assigned to.
const (
	maxPlugs = 32
	maxBulbs = 16
)

// forbiddenCooldown is how long to leave a device alone after it refused
// local access with HTTP 403: Tapo firmware locks out the local API after
// repeated handshake attempts, and retrying immediately can keep the
// lockout alive indefinitely.
const forbiddenCooldown = 30 * time.Minute

// isForbidden reports whether the error is a device lockout (HTTP 403). The
// KLAP handshake1 path reports the 403 with its own error string instead of
// tapo.ErrForbidden, hence the string check.
func isForbidden(err error) bool {
	if errors.Is(err, tapo.ErrForbidden) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "403 Forbidden")
}

var (
	deviceItems      []*DeviceItem
	deviceItemsMutex sync.Mutex
	// rebuildMutex serializes device list rebuilds and state refreshes, so
	// that switching source while a rebuild is in flight cannot interleave
	// results.
	rebuildMutex sync.Mutex

	plugPool *slotPool
	bulbPool *slotPool

	// mEnergy is the "Energy usage" submenu, with one entry per plug that
	// supports power monitoring. Hidden while empty.
	mEnergy     *systray.MenuItem
	energyMutex sync.Mutex
	energyCount int
)

// MenuSlot is a pre-created menu item that a device can be assigned to.
type MenuSlot struct {
	item *systray.MenuItem
	mu   sync.Mutex
	di   *DeviceItem
}

// slotPool is a menu section: a disabled header item followed by a fixed
// number of slots.
type slotPool struct {
	name  string
	slots []*MenuSlot
}

func newSlotPool(cfg *Config, name string, size int) *slotPool {
	header := systray.AddMenuItem(name, name)
	header.Disable()
	p := slotPool{name: name}
	for range size {
		s := &MenuSlot{item: systray.AddMenuItemCheckbox("", "", false)}
		s.item.Hide()
		go func(s *MenuSlot) {
			for range s.item.ClickedCh {
				s.mu.Lock()
				di := s.di
				s.mu.Unlock()
				if di != nil {
					di.toggle(cfg)
				}
			}
		}(s)
		p.slots = append(p.slots, s)
	}
	return &p
}

// assign binds a device to the first free slot of the pool and shows its
// menu item. It returns false if all the slots are taken.
func (p *slotPool) assign(di *DeviceItem) bool {
	for _, s := range p.slots {
		s.mu.Lock()
		if s.di != nil {
			s.mu.Unlock()
			continue
		}
		s.di = di
		di.slot = s
		di.title = di.label()
		s.item.SetTitle(di.title)
		s.item.SetTooltip(fmt.Sprintf("Toggle the Tapo %s at %s", di.kind, di.addr))
		s.item.Uncheck()
		s.item.Show()
		s.mu.Unlock()
		return true
	}
	return false
}

func addEnergyLine(title string) *systray.MenuItem {
	energyMutex.Lock()
	defer energyMutex.Unlock()
	item := mEnergy.AddSubMenuItem(title, "")
	item.Disable()
	energyCount++
	if energyCount == 1 {
		mEnergy.Show()
	}
	return item
}

func removeEnergyLine(item *systray.MenuItem) {
	energyMutex.Lock()
	defer energyMutex.Unlock()
	item.Remove()
	energyCount--
	if energyCount == 0 {
		mEnergy.Hide()
	}
}

// DeviceItem ties a Tapo device, configured or found via a source, to its
// menu slot.
type DeviceItem struct {
	slot       *MenuSlot
	addr       string
	kind       deviceKind
	fromConfig bool

	// mu guards the fields below and all device operations, so that a
	// toggle and a periodic refresh cannot run concurrently on the same
	// device.
	mu   sync.Mutex
	name string
	plug *tapo.Plug
	// on is the last known power state of the device.
	on bool
	// title is the text of the device's menu item, kept so that the item can
	// be redrawn when the device moves to another slot.
	title string
	// energySupport is nil until it is known whether the device supports
	// power monitoring.
	energySupport *bool
	energyItem    *systray.MenuItem
	// blockedUntil is the end of the cooldown after the device refused
	// local access with HTTP 403. Background refreshes are skipped until
	// then; manual toggles still go through.
	blockedUntil time.Time
}

// label must be called with d.mu held.
func (d *DeviceItem) label() string {
	if d.name != "" {
		return d.name
	}
	return d.addr
}

// withItem runs fn on the device's menu item, unless the device has been
// removed from the menu, in which case it returns false.
func (d *DeviceItem) withItem(fn func(item *systray.MenuItem)) bool {
	d.slot.mu.Lock()
	defer d.slot.mu.Unlock()
	if d.slot.di != d {
		return false
	}
	fn(d.slot.item)
	return true
}

// release unbinds the device from its menu slot, hiding the menu item, and
// removes its energy usage entry if any.
func (d *DeviceItem) release() {
	d.slot.mu.Lock()
	if d.slot.di == d {
		d.slot.di = nil
		d.slot.item.Hide()
		d.slot.item.Uncheck()
	}
	d.slot.mu.Unlock()
	d.mu.Lock()
	if d.energyItem != nil {
		removeEnergyLine(d.energyItem)
		d.energyItem = nil
	}
	d.mu.Unlock()
}

// klapLogger is like Config.tapoLogger, but never nil: unlike tapo.NewPlug,
// tapo.NewKlapSession does not handle a nil logger.
func klapLogger(cfg *Config) *log.Logger {
	if l := cfg.tapoLogger(); l != nil {
		return l
	}
	return log.New(io.Discard, "", 0)
}

// noteHandshakeError is like noteResult for handshake failures.
// Plug.Handshake hides the KLAP error behind its passthrough fallback, which
// KLAP-only devices always answer with error 1003, so a lockout can only be
// told apart from other failures with a diagnostic KLAP handshake. Must be
// called with d.mu held.
func (d *DeviceItem) noteHandshakeError(cfg *Config, err error) {
	if !isForbidden(err) && strings.Contains(err.Error(), "Communication error") {
		kerr := tapo.NewKlapSession(klapLogger(cfg)).Handshake(d.addr, cfg.Email, cfg.Password)
		if isForbidden(kerr) {
			err = kerr
		}
	}
	d.noteResult(err)
}

// noteResult updates the lockout state based on the outcome of a device
// operation. Must be called with d.mu held.
func (d *DeviceItem) noteResult(err error) {
	if err == nil {
		if !d.blockedUntil.IsZero() {
			logrus.Infof("%s %q (%s) accepts local access again", d.kind, d.label(), d.addr)
			d.blockedUntil = time.Time{}
		}
		return
	}
	if isForbidden(err) {
		d.blockedUntil = time.Now().Add(forbiddenCooldown)
		logrus.Errorf("%s %q (%s) refused local access (HTTP 403), backing off until %s to avoid keeping it locked out. Power-cycle the device to clear the block",
			d.kind, d.label(), d.addr, d.blockedUntil.Format("15:04"))
	}
}

// withSession runs fn on the device. Sessions expire after a while, so on
// failure it re-establishes the session and retries once — unless the device
// reports a lockout (HTTP 403), in which case it backs off instead of
// hammering it with more handshakes. Must be called with d.mu held.
func (d *DeviceItem) withSession(cfg *Config, fn func(*tapo.Plug) error) error {
	if d.plug == nil {
		plug := tapo.NewPlug(d.addr, cfg.tapoLogger())
		if err := plug.Handshake(cfg.Email, cfg.Password); err != nil {
			d.noteHandshakeError(cfg, err)
			return fmt.Errorf("handshake failed: %w", err)
		}
		d.plug = plug
	}
	err := fn(d.plug)
	if err == nil {
		d.noteResult(nil)
		return nil
	}
	if isForbidden(err) {
		d.noteResult(err)
		d.plug = nil
		return err
	}
	plug := tapo.NewPlug(d.addr, cfg.tapoLogger())
	if herr := plug.Handshake(cfg.Email, cfg.Password); herr != nil {
		d.noteHandshakeError(cfg, herr)
		d.plug = nil
		return err
	}
	d.plug = plug
	err = fn(d.plug)
	d.noteResult(err)
	return err
}

func kwh(wattHours int) string {
	return fmt.Sprintf("%.2f kWh", float64(wattHours)/1000.0)
}

// refreshEnergyLocked fetches the power consumption of a plug that may
// support power monitoring, updating its energy usage entry. It returns a
// suffix with the instant power for the plug's menu title. Must be called
// with d.mu held.
func (d *DeviceItem) refreshEnergyLocked(cfg *Config) string {
	var (
		energy *tapo.EnergyUsage
		usage  *tapo.DeviceUsage
	)
	err := d.withSession(cfg, func(plug *tapo.Plug) error {
		var err error
		if energy, err = plug.GetEnergyUsage(); err != nil {
			return err
		}
		usage, err = plug.GetDeviceUsage()
		return err
	})
	if err != nil {
		if d.energySupport == nil {
			// the device info was just fetched successfully, so a failure
			// here means the device has no power monitoring
			logrus.Infof("Plug %q (%s) does not support power monitoring", d.label(), d.addr)
			no := false
			d.energySupport = &no
		} else {
			logrus.Errorf("Failed to get power consumption of plug %q (%s): %v", d.label(), d.addr, err)
		}
		return ""
	}
	yes := true
	d.energySupport = &yes
	watts := float64(energy.CurrentPower) / 1000.0
	line := fmt.Sprintf("%s: %.1f W now — %s today, %s last 7 days, %s last 30 days",
		d.label(), watts,
		kwh(usage.PowerUsage.Today), kwh(usage.PowerUsage.Past7), kwh(usage.PowerUsage.Past30),
	)
	if d.energyItem == nil {
		// don't add an entry for a device that is being removed
		if d.withItem(func(*systray.MenuItem) {}) {
			d.energyItem = addEnergyLine(line)
		}
	} else {
		d.energyItem.SetTitle(line)
	}
	return fmt.Sprintf(" (%.1f W)", watts)
}

// refreshLocked fetches the device state and updates the menu item
// accordingly. During a lockout cooldown it only updates the menu item,
// without contacting the device. Must be called with d.mu held.
func (d *DeviceItem) refreshLocked(cfg *Config) {
	if time.Now().Before(d.blockedUntil) {
		d.withItem(func(item *systray.MenuItem) {
			d.title = fmt.Sprintf("%s (locked out)", d.label())
			item.SetTitle(d.title)
			item.SetTooltip(fmt.Sprintf(
				"The device refused local access (HTTP 403); retrying after %s. Power-cycle it to clear the block, then click to retry",
				d.blockedUntil.Format("15:04"),
			))
		})
		return
	}
	var info *tapo.DeviceInfo
	err := d.withSession(cfg, func(plug *tapo.Plug) error {
		var err error
		info, err = plug.GetDeviceInfo()
		return err
	})
	if err != nil {
		logrus.Errorf("Failed to get state of %s %q (%s): %v", d.kind, d.label(), d.addr, err)
		suffix := "unreachable"
		if isForbidden(err) {
			suffix = "locked out"
		}
		d.withItem(func(item *systray.MenuItem) {
			d.title = fmt.Sprintf("%s (%s)", d.label(), suffix)
			item.SetTitle(d.title)
		})
		requestWindowUpdate()
		return
	}
	if d.name == "" && info.DecodedNickname != "" {
		d.name = info.DecodedNickname
	}
	suffix := ""
	if d.kind == kindPlug && (d.energySupport == nil || *d.energySupport) {
		suffix = d.refreshEnergyLocked(cfg)
	}
	d.on = info.DeviceON
	requestWindowUpdate()
	d.withItem(func(item *systray.MenuItem) {
		d.title = d.label() + suffix
		item.SetTitle(d.title)
		item.SetTooltip(fmt.Sprintf("Toggle the Tapo %s at %s", d.kind, d.addr))
		if info.DeviceON {
			item.Check()
		} else {
			item.Uncheck()
		}
	})
}

func (d *DeviceItem) refresh(cfg *Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshLocked(cfg)
}

// toggle turns the device on if its checkbox is unchecked and off otherwise,
// then refreshes the checkbox with the actual device state.
func (d *DeviceItem) toggle(cfg *Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.withItem(func(*systray.MenuItem) {}) {
		return
	}
	turnOn := !d.on
	logrus.Infof("Turning %s %s %q (%s)", onOff(turnOn), d.kind, d.label(), d.addr)
	err := d.withSession(cfg, func(plug *tapo.Plug) error {
		return plug.SetDeviceInfo(turnOn)
	})
	if err != nil {
		logrus.Errorf("Failed to turn %s %s %q (%s): %v", onOff(turnOn), d.kind, d.label(), d.addr, err)
	}
	d.refreshLocked(cfg)
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// addDevice assigns the given device to a slot in the section of its kind.
// Devices whose address is already present are skipped.
func addDevice(dc DeviceConfig, fromConfig bool) {
	deviceItemsMutex.Lock()
	defer deviceItemsMutex.Unlock()
	for _, di := range deviceItems {
		if di.addr == dc.Address {
			return
		}
	}
	di := &DeviceItem{name: dc.Name, addr: dc.Address, kind: dc.kind(), fromConfig: fromConfig}
	pool := plugPool
	if di.kind == kindBulb {
		pool = bulbPool
	}
	if !pool.assign(di) {
		logrus.Warningf("Too many devices in section %q, cannot add %q (%s)", pool.name, di.label(), di.addr)
		return
	}
	deviceItems = append(deviceItems, di)
	requestWindowUpdate()
}

// sortSlots reorders the menu items of the pool by device name, so that the
// menu lists the devices alphabetically whatever the order they were found
// in. The slots stay where they are; the devices move between them. Must be
// called with rebuildMutex held.
func sortSlots(p *slotPool, kind deviceKind) {
	deviceItemsMutex.Lock()
	var devices []*DeviceItem
	for _, di := range deviceItems {
		if di.kind == kind {
			devices = append(devices, di)
		}
	}
	deviceItemsMutex.Unlock()
	if len(devices) < 2 {
		return
	}
	// hold the lock of every device, so that no toggle or refresh uses a slot
	// while it is handed over. Other code holds one device lock at a time.
	type keyed struct {
		di    *DeviceItem
		label string
	}
	var sorted []keyed
	for _, di := range devices {
		di.mu.Lock()
		defer di.mu.Unlock()
		sorted = append(sorted, keyed{di, di.label()})
	}
	slices.SortStableFunc(sorted, func(a, b keyed) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.label), strings.ToLower(b.label)),
			cmp.Compare(a.label, b.label),
		)
	})
	var slots []*MenuSlot
	for _, s := range p.slots {
		s.mu.Lock()
		occupied := s.di != nil
		s.mu.Unlock()
		if occupied {
			slots = append(slots, s)
		}
	}
	if len(slots) != len(sorted) {
		return
	}
	for i, k := range sorted {
		s := slots[i]
		s.mu.Lock()
		s.di = k.di
		k.di.slot = s
		s.item.SetTitle(k.di.title)
		s.item.SetTooltip(fmt.Sprintf("Toggle the Tapo %s at %s", k.di.kind, k.di.addr))
		if k.di.on {
			s.item.Check()
		} else {
			s.item.Uncheck()
		}
		s.mu.Unlock()
	}
}

// removeDynamicDevices removes all the devices that don't come from the
// configuration file.
func removeDynamicDevices() {
	deviceItemsMutex.Lock()
	defer deviceItemsMutex.Unlock()
	kept := deviceItems[:0]
	for _, di := range deviceItems {
		if di.fromConfig {
			kept = append(kept, di)
		} else {
			di.release()
		}
	}
	deviceItems = kept
	requestWindowUpdate()
}

// kindOfDeviceType maps a Tapo device type like "SMART.TAPOPLUG" to a device
// kind, returning false for unsupported device types.
func kindOfDeviceType(deviceType string) (deviceKind, bool) {
	t := strings.ToUpper(deviceType)
	switch {
	case strings.Contains(t, "PLUG"):
		return kindPlug, true
	case strings.Contains(t, "BULB"):
		return kindBulb, true
	}
	return kindPlug, false
}

// discoverDevices looks for Tapo devices on the local network via UDP
// broadcast discovery, and returns them as device configurations with no
// name, so that their nickname is used as label.
func discoverDevices(cfg *Config) []DeviceConfig {
	logrus.Infof("Discovering Tapo devices on the local network")
	client := tapo.NewClient(cfg.tapoLogger())
	devices, _, err := client.Discover()
	if err != nil {
		logrus.Errorf("Discovery failed: %v", err)
		return nil
	}
	var ret []DeviceConfig
	for _, dev := range devices {
		kind, ok := kindOfDeviceType(dev.Result.DeviceType)
		if !ok {
			logrus.Infof("Skipping unsupported device %s (%s) at %s",
				dev.Result.DeviceModel, dev.Result.DeviceType, dev.Result.IP.String())
			continue
		}
		logrus.Infof("Discovered %s %s at %s", kind, dev.Result.DeviceModel, dev.Result.IP.String())
		ret = append(ret, DeviceConfig{Address: dev.Result.IP.String(), Type: kind.String()})
	}
	return ret
}

// arpTable returns a MAC-to-IP map of the system's IPv4 neighbor table. Only
// implemented on Linux.
func arpTable() (map[string]string, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("ARP table lookup is not implemented on %s", runtime.GOOS)
	}
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	ret := make(map[string]string)
	lines := strings.Split(string(data), "\n")
	// the first line is the header
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		ip, flags, mac := fields[0], fields[2], fields[3]
		// 0x0 means incomplete entry
		if flags == "0x0" || mac == "00:00:00:00:00:00" {
			continue
		}
		ret[strings.ToLower(mac)] = ip
	}
	return ret, nil
}

// cloudDevices gets the list of devices known to the TP-Link cloud account.
// The cloud list contains no local addresses, so each device's MAC is
// resolved via the system's ARP table; devices without an ARP entry are
// skipped.
func cloudDevices(cfg *Config) []DeviceConfig {
	logrus.Infof("Getting the device list from the TP-Link cloud")
	client := tapo.NewClient(cfg.tapoLogger())
	if err := client.CloudLogin(cfg.Email, cfg.Password); err != nil {
		logrus.Errorf("Cloud login failed: %v", err)
		return nil
	}
	devices, err := client.CloudList()
	if err != nil {
		logrus.Errorf("Failed to get cloud device list: %v", err)
		return nil
	}
	arp, err := arpTable()
	if err != nil {
		logrus.Errorf("Cannot resolve device addresses: %v", err)
		return nil
	}
	var ret []DeviceConfig
	for _, dev := range devices {
		kind, ok := kindOfDeviceType(dev.DeviceType)
		if !ok {
			logrus.Infof("Skipping unsupported device %q (%s)", dev.DecodedAlias, dev.DeviceType)
			continue
		}
		mac := strings.ToLower(dev.DeviceMAC.String())
		ip, found := arp[mac]
		if !found {
			logrus.Warningf("No ARP entry for %s %q (%s), cannot determine its address, skipping. The device must have exchanged traffic with this host recently", kind, dev.DecodedAlias, mac)
			continue
		}
		logrus.Infof("Cloud %s %q (%s) is at %s", kind, dev.DecodedAlias, mac, ip)
		ret = append(ret, DeviceConfig{Name: dev.DecodedAlias, Address: ip, Type: kind.String()})
	}
	return ret
}

// refreshAllLocked refreshes the state of all devices in parallel. Must be
// called with rebuildMutex held.
func refreshAllLocked(cfg *Config) {
	deviceItemsMutex.Lock()
	devices := make([]*DeviceItem, len(deviceItems))
	copy(devices, deviceItems)
	deviceItemsMutex.Unlock()
	var wg sync.WaitGroup
	for _, di := range devices {
		wg.Add(1)
		go func(di *DeviceItem) {
			defer wg.Done()
			di.refresh(cfg)
		}(di)
	}
	wg.Wait()
	sortSlots(plugPool, kindPlug)
	sortSlots(bulbPool, kindBulb)
}

func refreshAll(cfg *Config) {
	rebuildMutex.Lock()
	defer rebuildMutex.Unlock()
	refreshAllLocked(cfg)
}

// rebuildDevices replaces the dynamically found devices with the ones found
// via the given source, keeping the configured ones, then refreshes all the
// device states.
func rebuildDevices(cfg *Config, src deviceSource) {
	rebuildMutex.Lock()
	defer rebuildMutex.Unlock()
	removeDynamicDevices()
	var found []DeviceConfig
	switch src {
	case sourceDiscover:
		found = discoverDevices(cfg)
	case sourceCloud:
		found = cloudDevices(cfg)
	}
	for _, dc := range found {
		addDevice(dc, false)
	}
	refreshAllLocked(cfg)
}

func onReady(cfg *Config) {
	systray.SetIcon(Icon)
	systray.SetTitle("Tapo")
	systray.SetTooltip("Control Tapo smart devices")

	mRefresh := systray.AddMenuItem("Refresh now", "Fetch the current state of all devices")
	mUpdate := systray.AddMenuItem("Update device list now", "Search again for devices using the selected method")
	mSrcDiscover := systray.AddMenuItemCheckbox(
		"Find devices via local discovery",
		"Find devices on the local network via UDP broadcast discovery",
		cfg.Discover,
	)
	mSrcCloud := systray.AddMenuItemCheckbox(
		"Find devices via TP-Link cloud",
		"Get the list of devices from the TP-Link cloud account",
		cfg.CloudList,
	)
	mWindow := systray.AddMenuItem("Show window", "Open a window with a button per device")
	mEnergy = systray.AddMenuItem("Energy usage", "Power consumption of the plugs that support monitoring")
	mEnergy.Hide()
	if cfg.updateInterval > 0 {
		mInterval := systray.AddMenuItem(
			fmt.Sprintf("Device states will be refreshed every %s", cfg.updateInterval),
			fmt.Sprintf("The device states will be refreshed every %s", cfg.updateInterval),
		)
		mInterval.Disable()
	}
	mQuit := systray.AddMenuItem("Quit", "Quit the app")
	systray.AddSeparator()
	plugPool = newSlotPool(cfg, "Plugs", maxPlugs)
	systray.AddSeparator()
	bulbPool = newSlotPool(cfg, "Bulbs", maxBulbs)

	activeSource := func() deviceSource {
		switch {
		case mSrcDiscover.Checked():
			return sourceDiscover
		case mSrcCloud.Checked():
			return sourceCloud
		}
		return sourceNone
	}

	// the configured devices are added right away, the found ones once the
	// selected source has completed.
	for _, dc := range cfg.Devices {
		addDevice(dc, true)
	}
	go rebuildDevices(cfg, activeSource())

	go func() {
		var (
			timer       *time.Ticker
			ignoreTimer = false
		)
		if cfg.updateInterval > 0 {
			timer = time.NewTicker(cfg.updateInterval)
			logrus.Infof("Refreshing device states every %s", cfg.updateInterval)
		} else {
			// a non-positive interval means "don't refresh in background".
			// This creates a ticker with a valid time, but it will be ignored
			timer = time.NewTicker(time.Hour)
			ignoreTimer = true
		}
		for {
			select {
			case <-mQuit.ClickedCh:
				systray.Quit()
				quitWindow()
			case <-mWindow.ClickedCh:
				showWindow()
			case <-mRefresh.ClickedCh:
				refreshAll(cfg)
			case <-mUpdate.ClickedCh:
				rebuildDevices(cfg, activeSource())
			case <-mSrcDiscover.ClickedCh:
				// the two sources are mutually exclusive: checking one
				// unchecks the other. Unchecking both leaves only the
				// configured devices.
				if mSrcDiscover.Checked() {
					mSrcDiscover.Uncheck()
				} else {
					mSrcDiscover.Check()
					mSrcCloud.Uncheck()
				}
				rebuildDevices(cfg, activeSource())
			case <-mSrcCloud.ClickedCh:
				if mSrcCloud.Checked() {
					mSrcCloud.Uncheck()
				} else {
					mSrcCloud.Check()
					mSrcDiscover.Uncheck()
				}
				rebuildDevices(cfg, activeSource())
			case <-timer.C:
				if !ignoreTimer {
					refreshAll(cfg)
				}
			}
		}
	}()
}

func onExit() {
	// clean up here
}
