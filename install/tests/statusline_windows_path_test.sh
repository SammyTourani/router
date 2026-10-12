#!/usr/bin/env bash
#
# Under Git Bash, settings.json stores the statusline command in C:/ form while
# $HOME is /c/, so reinstall and uninstall must still recognise it as router-owned.
# shellcheck disable=SC2016  # the fake cygpath needs its own runtime expansions

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/weave"
printf '%s\n' '#!/usr/bin/env bash' \
  '[ "$1" = "-m" ] && [ "$2" = "--" ] || exit 2' \
  'case "$3" in /c/*) printf "C:%s\n" "${3#/c}" ;; *) printf "%s\n" "$3" ;; esac' >"$work/bin/cygpath"
chmod +x "$work/bin/cygpath"

posix_command="/c/Users/dev/.weave/cc-statusline.sh"
script_path="$work/weave/cc-statusline.sh"
settings="$work/settings.json"
printf '{"statusLine":{"type":"command","command":"C:/Users/dev/.weave/cc-statusline.sh"}}\n' >"$settings"
printf '#!/usr/bin/env bash\n# Claude Code statusline for the Weave Router.\n' >"$script_path"

pass=0
fail=0
check() {
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1)); echo "  ok   $1"
  else
    fail=$((fail + 1)); echo "  FAIL $1: expected [$2], got [$3]"
  fi
}

for script in install.sh uninstall.sh; do
  # Load just the ownership helpers so cygpath and the paths can be stubbed.
  eval "$(sed -n '/^CLAUDE_STATUSLINE_MARKER=/p' "$script_dir/../$script")"
  eval "$(awk '/^statusline_commands_match\(\)/,/^}/' "$script_dir/../$script")"
  eval "$(awk '/^claude_statusline_router_owned\(\)/,/^}/' "$script_dir/../$script")"

  rc=0
  PATH="$work/bin:$PATH" claude_statusline_router_owned \
    "$settings" "$posix_command" "$script_path" "$script_path.weave-router" || rc=$?
  check "$script owns a C:/ statusline command when \$HOME is /c/" "0" "$rc"

  rc=0
  PATH="$work/bin:$PATH" claude_statusline_router_owned \
    "$settings" "/c/Users/other/.weave/cc-statusline.sh" "$script_path" "$script_path.weave-router" || rc=$?
  check "$script still rejects a different statusline path" "1" "$rc"
done

echo
echo "$pass passed"
[ "$fail" -eq 0 ]
