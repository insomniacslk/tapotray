PREFIX ?= $(HOME)/.local

.PHONY: build install autostart uninstall clean

build:
	go build

install: build
	install -Dm755 tapotray $(PREFIX)/bin/tapotray
	install -dm755 $(PREFIX)/share/applications
	sed 's|^Exec=.*|Exec=$(PREFIX)/bin/tapotray|' tapotray.desktop > $(PREFIX)/share/applications/tapotray.desktop
	install -Dm644 icon.png $(PREFIX)/share/icons/hicolor/64x64/apps/tapotray.png
	install -Dm644 icon.svg $(PREFIX)/share/icons/hicolor/scalable/apps/tapotray.svg

# start tapotray automatically when logging into the desktop session
autostart: install
	install -Dm644 $(PREFIX)/share/applications/tapotray.desktop $(HOME)/.config/autostart/tapotray.desktop

uninstall:
	rm -f $(PREFIX)/bin/tapotray
	rm -f $(PREFIX)/share/applications/tapotray.desktop
	rm -f $(PREFIX)/share/icons/hicolor/64x64/apps/tapotray.png
	rm -f $(PREFIX)/share/icons/hicolor/scalable/apps/tapotray.svg
	rm -f $(HOME)/.config/autostart/tapotray.desktop

clean:
	rm -f tapotray
