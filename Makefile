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
	launchctl kickstart -k gui/$$(id -u)/zip.connor.acme-hotkey

deploy: install restart
