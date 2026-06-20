#!/bin/sh

# SPDX-FileCopyrightText: Copyright The Lima Authors
# SPDX-License-Identifier: Apache-2.0

# Import host USB devices exported by the host USB/IP server over vsock.
# The guest agent speaks USB/IP directly — no usbip/socat packages needed.

set -eu

[ "${LIMA_CIDATA_VMTYPE}" = "vz" ] || exit 0
{ [ "${LIMA_CIDATA_USB_ENABLED:-0}" = "1" ] || [ "${LIMA_CIDATA_USB_DEVICES:-0}" -gt 0 ]; } || exit 0

modprobe vhci-hcd 2>/dev/null || true

GA="$(command -v lima-guestagent || echo "${LIMA_CIDATA_GUEST_INSTALL_PREFIX:-/usr/local}/bin/lima-guestagent")"
if [ -x "${GA}" ]; then
	"${GA}" usbip attach --all || echo >&2 "usbip: auto-attach failed (negligible if no devices are allowlisted)"
fi
