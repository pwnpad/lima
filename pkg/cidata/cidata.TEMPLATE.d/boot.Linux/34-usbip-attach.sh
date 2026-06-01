#!/bin/sh

# SPDX-FileCopyrightText: Copyright The Lima Authors
# SPDX-License-Identifier: Apache-2.0

# Import host USB devices exported by the limactl USB/IP server. The host
# serves the devices over vsock (host CID 2, port 2223); the stock usbip
# client only speaks TCP, so socat bridges a local TCP port to the vsock.

set -eu

[ "${LIMA_CIDATA_USB_DEVICES:-0}" -gt 0 ] || exit 0

HOST_CID=2
VSOCK_PORT=2223
TCP_PORT=3240

install_pkgs() {
	if command -v apt-get >/dev/null 2>&1; then
		DEBIAN_FRONTEND=noninteractive
		export DEBIAN_FRONTEND
		apt-get update
		apt-get install -y --no-install-recommends socat usbip "linux-modules-extra-$(uname -r)" || true
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y socat usbip || true
	elif command -v apk >/dev/null 2>&1; then
		apk add socat usbip-tools || true
	else
		echo >&2 "usbip: no supported package manager found; install socat and usbip manually"
	fi
}

if ! command -v socat >/dev/null 2>&1 || ! command -v usbip >/dev/null 2>&1; then
	install_pkgs
fi

if ! command -v socat >/dev/null 2>&1 || ! command -v usbip >/dev/null 2>&1; then
	echo >&2 "usbip: socat and/or usbip still missing, skipping USB passthrough"
	exit 0
fi

modprobe vhci-hcd 2>/dev/null || true

# Start the TCP<->vsock bridge once.
if ! pgrep -f "TCP-LISTEN:${TCP_PORT}.*VSOCK-CONNECT:${HOST_CID}:${VSOCK_PORT}" >/dev/null 2>&1; then
	socat "TCP-LISTEN:${TCP_PORT},fork,reuseaddr" "VSOCK-CONNECT:${HOST_CID}:${VSOCK_PORT}" &
fi

# Wait for the bridge and server to answer a device list request.
for _ in $(seq 1 15); do
	if usbip list -r 127.0.0.1 >/dev/null 2>&1; then
		break
	fi
	sleep 1
done

busids=$(usbip list -pr 127.0.0.1 2>/dev/null | sed -n 's/^busid=\([^#]*\)#.*/\1/p')
if [ -z "${busids}" ]; then
	echo >&2 "usbip: no exportable devices found on the host server"
	exit 0
fi

for b in ${busids}; do
	echo "usbip: attaching ${b}"
	if ! usbip attach -r 127.0.0.1 -b "${b}"; then
		echo >&2 "usbip: failed to attach ${b}"
	fi
done
