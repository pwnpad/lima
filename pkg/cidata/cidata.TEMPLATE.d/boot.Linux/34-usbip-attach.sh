#!/bin/sh

# SPDX-FileCopyrightText: Copyright The Lima Authors
# SPDX-License-Identifier: Apache-2.0

# Import host USB devices exported by the limactl USB/IP server. The guest agent
# speaks the USB/IP protocol directly to the host over vsock (host CID 2, port
# 2223) and hands the connected socket to vhci-hcd, so no usbip/socat packages
# are needed in the guest — only the vhci-hcd kernel module.

set -eu

# USB/IP passthrough is a vz-only feature; the host USB/IP server is started by
# the vz driver. qemu handles USB passthrough itself, so skip there.
[ "${LIMA_CIDATA_VMTYPE}" = "vz" ] || exit 0
# Run whenever vz USB is enabled (usb: true) or any devices are configured, so a
# later live `limactl usb attach` has vhci-hcd ready.
{ [ "${LIMA_CIDATA_USB_ENABLED:-0}" = "1" ] || [ "${LIMA_CIDATA_USB_DEVICES:-0}" -gt 0 ]; } || exit 0

modprobe vhci-hcd 2>/dev/null || true

# Auto-attach any devices already advertised by the host (allowlist/usbDevices).
# Best effort: the agent may not be installed yet this early in boot; live
# `limactl usb attach` works regardless once it is.
GA="$(command -v lima-guestagent || echo "${LIMA_CIDATA_GUEST_INSTALL_PREFIX:-/usr/local}/bin/lima-guestagent")"
if [ -x "${GA}" ]; then
	"${GA}" usbip attach --all || echo >&2 "usbip: auto-attach failed (negligible if no devices are allowlisted)"
fi
