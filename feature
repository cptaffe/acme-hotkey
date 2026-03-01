# Acme Hotkey

Write a new daemon service in Go in the style of skhd, which intercepts keyboard
input before it reaches Acme. Keypresses are matched against a trie constructed
from hotkey rules.

Acme Hotkey serves a 9P filesystem (see acme-styles for approach), which presents
the trie via the path within the filesystem tree, walking the tree traverses the
trie to find a match -- the paths are not known until a traversal is done, so
the list operation is not supported.

acme-hotkeys/
	route/
		# a classic command+shift+b hotkey
		command/
			-/
				shift/
					-/
						b
		# an emacs-style C-x left sequence, which would switch to the previous buffer
		ctrl/
			-/
				x/
					,/
						left
	routes

Directories represent non-terminals and terminals are files. The file contains the
command text associated with the hotkey, e.g.:

% 9p read acme-hotkeys/command/shift/h
Highlight

The current window in focus is tracked by reading acme/log's focus entries,
when a key sequence such as command+shift+h matches, the associated command e.g.
Highlight is found in the currently in focus window's tag, or written there, and
then an Mx event is sent referencing the rune offsets of the text, executing it
in the current window as if it was button 2 clicked.

The routes file is readable and writable, and represents the live set of routes,
seeded from the config (-config) when acme-hotkey was sarted. It supports
structural regular expressions 
