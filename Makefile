BINARY   := $(HOME)/go/bin/acme-hotkey
IDENTITY := zip.connor.acme-hotkey

.PHONY: install sign restart all

all: install

install:
	GOWORK=off go install .
	$(MAKE) sign

sign:
	codesign --sign - --force --identifier "$(IDENTITY)" "$(BINARY)"

restart:
	launchctl kickstart -k gui/$$(id -u)/zip.connor.acme-hotkey

deploy: install restart
