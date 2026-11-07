#!/bin/sh
set -eu
if [ "$(id -u)" -ne 0 ]; then echo '请由电脑主人以管理员权限运行此卸载脚本。' >&2; exit 1; fi
/bin/launchctl bootout system/org.clearline.tunnel >/dev/null 2>&1 || true
if [ -e /var/run/org.clearline.tunnel.state ]; then
    echo '网络恢复记录仍存在，请先通过 Clearline 恢复网络，再卸载组件。' >&2
    exit 1
fi
rm -f /Library/LaunchDaemons/org.clearline.tunnel.plist /Library/PrivilegedHelperTools/org.clearline.tunnel /Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist
/usr/sbin/pkgutil --forget org.clearline.tunnel.pkg >/dev/null 2>&1 || true
