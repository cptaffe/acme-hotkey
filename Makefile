BINARY   := $(HOME)/go/bin/acme-hotkey
IDENTITY := LocalCodeSign

# NOTE: always use 'make install' (not 'go build' or 'go install' alone).
#
# macOS TCC (Accessibility permission) tracks by code-signing identity
# (identifier + certificate), not by inode or binary hash.  'go install'
# produces an ad-hoc signed binary with identifier "a.out"; TCC does not
# recognise that as the same identity as "zip.connor.acme-hotkey" and
# silently revokes the Accessibility permission, causing CGEventTapCreate
# to fail on the next run.
#
# 'make install' rebuilds and then re-signs with the LocalCodeSign
# certificate, preserving the stable identity.  TCC keeps the permission
# across rebuilds as long as the identity is consistent.

.PHONY: install sign restart deploy all

all: install

install:
	GOWORK=off go install .
	$(MAKE) sign

sign:
	codesign --sign "$(IDENTITY)" --force --identifier "zip.connor.acme-hotkey" \
	  --timestamp=none "$(BINARY)"

# Restart via 9init (the authoritative supervisor).
# If the service is in a failed state, 'start' clears the budget first.
restart:
	echo 'restart acme-hotkey' | 9p write init/ctl

deploy: install restart
