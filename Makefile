BINARY   := $(HOME)/go/bin/acme-hotkey
IDENTITY := LocalCodeSign

.PHONY: install sign restart all

all: install

install:
	GOWORK=off go install .
	$(MAKE) sign

sign:
	codesign --sign "$(IDENTITY)" --force --identifier "zip.connor.acme-hotkey" \
	  --timestamp=none "$(BINARY)"

restart:
	launchctl unload $(HOME)/Library/LaunchAgents/zip.connor.acme-hotkey.plist 2>/dev/null; true
	launchctl load  $(HOME)/Library/LaunchAgents/zip.connor.acme-hotkey.plist

deploy: install restart
