package vm

import (
	"io"
	"strconv"
	"strings"
)

// onInputCmd is the command that runs the script that comes first on its
// input, as onInput frames it: a line with the script's length in bytes,
// then the script, then the rest of the input, which the script gets as
// its own. It reads exactly that many bytes (dd bs=1, so not one byte of
// the rest), and runs the script only when it read them all, so an input
// cut short runs nothing, and says so on its stderr, which goes to the
// VM's log.
const onInputCmd = `if IFS= read -r n && s=$(dd bs=1 count="$n" 2>/dev/null) && [ "$(printf %s "$s" | wc -c)" -eq "$n" ] 2>/dev/null; then exec bash -c "$s" bash; fi; echo "devvm: The script on the input was cut short, so it didn't run" >&2; exit 1`

// onInput returns the command and the input that run script in a VM with
// rest as its input: the script goes on the input, never in the command's
// arguments, as ssh's shared connection on macOS fails to start a session
// whose command is that long ("mm_send_fd: sendmsg: Message too long"),
// which an export's or an import of agents' history is. A script's
// trailing newlines go, as the shell's $(…) takes them off.
func onInput(script string, rest io.Reader) (argv []string, stdin io.Reader) {
	script = strings.TrimRight(script, "\n")
	in := strings.NewReader(strconv.Itoa(len(script)) + "\n" + script)
	if rest == nil {
		return []string{"bash", "-c", onInputCmd}, in
	}
	return []string{"bash", "-c", onInputCmd}, io.MultiReader(in, rest)
}
