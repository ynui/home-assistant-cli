# Regenerate demo/demo.gif:  make demo   (or: vhs demo/demo.tape from repo root)
#
# Runs bin/home-assistant-cli against bin/fakeha - a canned Home Assistant - with
# a throwaway config, so no real house and no real ~/.config/has are involved.
Output demo/demo.gif

Set Shell zsh
Set FontSize 20
Set Width 1200
Set Height 690
Set Margin 0
Set Padding 25
Set CursorBlink false
Set TypingSpeed 60ms
Set Theme {"name": "tokyonight", "background": "#16161e", "foreground": "#c0caf5", "cursor": "#c0caf5", "black": "#16161e", "red": "#f7768e", "green": "#9ece6a", "yellow": "#e0af68", "blue": "#7aa2f7", "magenta": "#bb9af7", "cyan": "#7dcfff", "white": "#a9b1d6", "brightBlack": "#414868", "brightRed": "#f7768e", "brightGreen": "#9ece6a", "brightYellow": "#e0af68", "brightBlue": "#7aa2f7", "brightMagenta": "#bb9af7", "brightCyan": "#7dcfff", "brightWhite": "#c0caf5"}

# sandbox: the fake house, then the TUI pointed at it
Hide
Type "cd `git rev-parse --show-toplevel`" Enter
Sleep 200ms
Type "./bin/fakeha &" Enter
Sleep 1500ms
# HAS_CONFIG has to name a *file*: configDir() takes its directory, and a bare
# temp directory would put ui.json one level up, shared with every other run.
Type "HAS_URL=http://127.0.0.1:8123 HAS_TOKEN=demo HAS_CONFIG=$(mktemp -d)/config.json ./bin/home-assistant-cli ui" Enter
Sleep 4s
Show

# the light group is folded: one keypress opens it
Right Sleep 1000ms
Down 4 Sleep 500ms
Space Sleep 1400ms
Down Sleep 400ms
Space Sleep 1400ms

# pin it: the row joins the starred favorites group at the top
Type "f" Sleep 1400ms

# everything the server knows about the highlighted entity
Ctrl+D Sleep 1600ms
Escape Sleep 700ms

# grouping: areas, states, flat, back to groups
Type "v" Sleep 1300ms
Type "v" Sleep 1300ms
Type "v" Sleep 1000ms
Type "v" Sleep 1000ms

# read-only entities are hidden until asked for
Ctrl+A Sleep 1300ms
Ctrl+A Sleep 800ms

# search is modal: / then every printable is filter text
Type "/" Sleep 600ms
Type "kit" Sleep 1200ms
Escape Sleep 1000ms

Type "?" Sleep 2000ms
Escape Sleep 800ms

# ctrl+c is the only exit
Ctrl+C Sleep 500ms