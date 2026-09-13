#!/usr/bin/env bash
# Shared helpers for Gas City release scripts.
#
# Source this file in other scripts:
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SCRIPT_DIR/lib/common.sh"

# shellcheck disable=SC2034  # colors are consumed by sourcing scripts
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

is_darwin_sed() {
    [[ "$OSTYPE" == "darwin"* && "$(command -v sed)" == "/usr/bin/sed" ]]
}

# Cross-platform `sed -i` wrapper (BSD sed on macOS needs an explicit empty backup arg).
sed_i() {
    if is_darwin_sed; then
        sed -i '' "$@"
    else
        sed -i "$@"
    fi
}

append_word_once() {
    local var_name="$1"
    local word="$2"
    local current="${!var_name:-}"

    case " $current " in
        *" $word "*)
            return 0
            ;;
    esac

    printf -v "$var_name" '%s' "${current:+$current }$word"
}

configure_linux_system_cgo_fallback() {
    [[ "${SYS_USR_CGO_FALLBACK:-1}" != "0" ]] || return 0

    local sys_usr_include="${SYS_USR_INCLUDE:-/usr/include}"
    local sys_usr_lib_root="${SYS_USR_LIB_ROOT:-/usr/lib}"
    local sys_usr_lib64_root="${SYS_USR_LIB64_ROOT:-/usr/lib64}"
    local cc_bin="${CC:-cc}"

    [[ -f "$sys_usr_include/unicode/uregex.h" ]] || return 0
    if "$cc_bin" -E -Wp,-v -x c /dev/null 2>&1 |
        sed 's/^[[:space:]]*//' |
        grep -F -x -q "$sys_usr_include"; then
        return 0
    fi

    append_word_once cgo_cppflags "-I$sys_usr_include"

    local -a candidates=()
    local arch dir seen_dirs=""
    while IFS= read -r arch; do
        [[ -n "$arch" ]] || continue
        candidates+=("$sys_usr_lib_root/$arch")
    done < <(
        {
            dpkg-architecture -q DEB_HOST_MULTIARCH 2>/dev/null || true
            "$cc_bin" -print-multiarch 2>/dev/null || true
        }
    )
    candidates+=("$sys_usr_lib64_root" "$sys_usr_lib_root")

    for dir in "${candidates[@]}"; do
        [[ -d "$dir" ]] || continue
        case " $seen_dirs " in
            *" $dir "*)
                continue
                ;;
        esac
        seen_dirs="${seen_dirs:+$seen_dirs }$dir"
        append_word_once cgo_ldflags "-L$dir"
    done
}

configure_cgo_platform_paths() {
    cgo_cppflags="${cgo_cppflags:-${CGO_CPPFLAGS:-}}"
    cgo_ldflags="${cgo_ldflags:-${CGO_LDFLAGS:-}}"

    case "$(uname)" in
        Darwin)
            local icu_prefix
            icu_prefix="$(brew --prefix icu4c 2>/dev/null || true)"
            if [[ -n "$icu_prefix" ]]; then
                append_word_once cgo_cppflags "-I${icu_prefix}/include"
                append_word_once cgo_ldflags "-L${icu_prefix}/lib"
            fi
            ;;
        Linux)
            configure_linux_system_cgo_fallback
            ;;
    esac
}
