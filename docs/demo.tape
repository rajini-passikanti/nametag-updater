# Run from the repository root: vhs docs/demo.tape
# Records a real fresh clone. Timestamps, PIDs, and machine paths are
# removed from make's displayed output. Repeated heartbeats and setup chatter
# are condensed for readability; the demo itself is unmodified.
Require git
Require go
Require make
Require bash
Require awk

Output docs/demo.gif
Output docs/demo.mp4
Output "/tmp/nametag-demo.ascii"

Set Shell bash
Set Width 1180
Set Height 900
Set FontFamily "Menlo"
Set FontSize 18
Set LineHeight 1.15
Set Padding 24
Set Theme "Catppuccin Mocha"
Set Framerate 10
Set CursorBlink false
Set TypingSpeed 30ms
Set WaitTimeout 60s
Env PS1 "$ "
Env GIT_TERMINAL_PROMPT "0"

Hide
Type@1ms `export PS1='$ '; set -o pipefail; demo_workspace=$(mktemp -d); cd "$demo_workspace"; make() { command make "$@" 2>&1 | awk '{ if ($0 ~ /^[[:space:]]*$/ || $0 ~ /^(private key:|public key:|\.\/scripts\/demo\.sh)/) next; if ($0 ~ /msg=heartbeat/) { if ($0 ~ /version=1.0.0/ && seen_old++) next; if ($0 ~ /version=1.1.0/ && seen_new++) next; } sub(/^time=[^ ]+ /, ""); sub(/level=INFO /, ""); gsub(/ (pid|exe|server)=[^ ]+/, ""); print; fflush(); }' ; }; clear`
Enter
Wait+Line /\$/
Show

Type "# 1. Clone the project (Go + Bash + Make required)"
Enter
Type "git clone https://github.com/rajini-passikanti/nametag-updater.git"
Enter
Wait+Line /\$/
Type "cd nametag-updater"
Enter
Sleep 1s
Hide
Type@1ms "clear"
Enter
Sleep 300ms
Show

Type "# 2. Run the self-update: 1.0.0 -> 1.1.0"
Enter
Type "make demo"
Enter
Wait+Screen /installed binary reports version: 1\.1\.0/
Wait+Line /\$/
Sleep 3s
Hide
Type@1ms "clear"
Enter
Sleep 300ms
Show

Type "# 3. Verify the installed version and confirmed state"
Enter
Type "./.demo/install/app --version"
Enter
Wait+Line /\$/
Sleep 1s
Type "cat .demo/install/.updater/state.json; echo"
Enter
Wait+Line /\$/
Sleep 4s

Hide
Type@1ms `cd /; test -n "$demo_workspace" && rm -rf -- "$demo_workspace"`
Enter
Sleep 500ms
