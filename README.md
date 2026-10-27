# tapotray

Control TP-Link Tapo smart plugs and bulbs via app indicator.

The tray menu has a section for plugs and one for bulbs. Each device appears
as a checkbox showing its current state; click it to toggle the device on or
off. The states are refreshed in background at the configured interval.

"Show window" in the tray menu opens an independent window with one button
per device, highlighted when the device is on. Closing it only hides it.

Plugs that support power monitoring (e.g. P110/P115) show their instant power
consumption next to their name, and the "Energy usage" submenu reports, for
each of them, the instant power and the energy used today, in the last 7
days, and in the last 30 days.

Devices can be listed explicitly in the configuration file, or found via one
of two methods, selectable with the checkboxes in the menu (mutually
exclusive, and both can be disabled to only use the configured devices):
* local discovery (`discover: true`): UDP broadcast discovery on the local
  network;
* TP-Link cloud (`cloud_list: true`): the device list of the TP-Link account.
  The cloud list contains no local addresses, so each device's MAC address is
  resolved via the system ARP table; devices without an ARP entry are skipped
  (Linux only).

Found devices are added to the configured ones. "Update device list now"
searches again with the selected method, e.g. for devices that appeared after
startup; switching method rebuilds the device list.

Build with
```
go build
```

Install the binary, desktop entry and icons under `~/.local` with
```
make install
```
(use `PREFIX=/usr/local sudo make install` for a system-wide install). To
start it automatically when logging into the desktop session, run
`make autostart`. `make uninstall` removes everything.

Create a configuration file at `~/.config/tapotray/config.yaml` with content similar
to [config.yaml.example](./config.yaml.example), or run `tapotray edit` to open
it in `EDITOR` (pre-filled with a template on first run) and validate it.

`email` and `password` are the credentials of your TP-Link/Tapo account (the
same ones used in the Tapo app); the devices use them for local
authentication. The cloud is only contacted when using `cloud_list`.
