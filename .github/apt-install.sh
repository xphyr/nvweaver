#!/usr/bin/env bash
# apt-install.sh PACKAGE...: installs packages on a hosted runner, without recommends, with retries.
#
# From about 14:00 UTC on 2026-11-11, azure.archive.ubuntu.com, the runners' first mirror, didn't resolve for some jobs, nor
# from some other places: it's a CNAME to an Azure Traffic Manager profile, which answers by region. Ubuntu's own archive
# is used instead when it doesn't, over IPv4, as it answers with IPv6 first. Arm runners use ports.ubuntu.com, which this
# leaves alone.
set -euo pipefail
if grep -rqs azure.archive.ubuntu.com /etc/apt/apt-mirrors.txt /etc/apt/sources.list /etc/apt/sources.list.d/ &&
	! getent hosts azure.archive.ubuntu.com >/dev/null; then
	echo "::warning::azure.archive.ubuntu.com doesn't resolve, so using archive.ubuntu.com"
	if [ -f /etc/apt/apt-mirrors.txt ]; then
		printf 'http://archive.ubuntu.com/ubuntu/\tpriority:1\n' | sudo tee /etc/apt/apt-mirrors.txt >/dev/null
	fi
	sudo sed -i 's#azure\.archive\.ubuntu\.com#archive.ubuntu.com#g' /etc/apt/sources.list /etc/apt/sources.list.d/* 2>/dev/null || true
fi
apt=(sudo apt-get -q -o Acquire::Retries=3 -o Acquire::http::Timeout=20 -o Acquire::ForceIPv4=true)
"${apt[@]}" update
"${apt[@]}" install -y --no-install-recommends "$@"
