#!/usr/bin/env bash
# build-base.sh — build the golden qcow2 (RARE; run to upgrade the base, not per test run).
# Live-provision approach: boot a stock Fedora cloud image, ssh in and run provision.sh (so
# containerd is actually running for image import), verify, shut down cleanly → the disk IS the
# golden image. More reliable than offline libguestfs image-import for containerd.
#
# Usage: OUT=/var/lib/libvirt/images/spawnery-golden.qcow2 scripts/e2e-vm/build-base.sh
# Prereqs: libvirtd + /dev/kvm, genisoimage/cloud-localds, the built spawnery binaries+images+web,
#          an ssh key at $E2E_SSH_KEY. Prints the golden path + the CA cert to install in host trust.
#
# STATUS: first draft — host-iterate. The system provisioning is concrete; the spawnery systemd env
# needs Justfile reconciliation (see provision.sh "RECONCILE").
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

: "${OUT:=/var/lib/libvirt/images/spawnery-e2e/golden.qcow2}"   # pool path (qemu-accessible)
: "${FEDORA_IMG_URL:=https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2}"
: "${FEDORA_IMG_SHA256:=28680fe5b371a5a82ebf43a31926e086a168e59949d03969c5093e7071f90b7f}"     # REQUIRED for reproducibility — pin the checksum
: "${BUILD_MEM_MB:=6144}"
: "${DISK_GB:=40}"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
BUILD_RUNID="golden-build-$$"

log "staging build payload (binaries + images + web + config + provisioner)…"
mkdir -p "$WORK/payload/bin" "$WORK/payload/config"
dbox() { distrobox enter --root dev-spawnery -- bash -lc "cd '$REPO_ROOT' && $*"; }
dbox "make build bin/spawnery_cp && cp -f bin/{spawnery_cp,authsvc,spawnlet,spawnctl,spawnery-ca} '$WORK/payload/bin/'"
dbox "make images && docker save spawnery/sidecar:dev spawnery/agent:dev -o '$WORK/payload/images.tar'" || warn "image build failed"
dbox "cd web && npm ci && VITE_CP_ORIGIN=https://placeholder.e2e.test VITE_AS_ORIGIN=https://placeholder.e2e.test npm run build" && cp -rf "$REPO_ROOT/web/dist" "$WORK/payload/web-dist" || warn "web build failed (non-fatal; roll.sh rebuilds per run)"
cp -rf "$REPO_ROOT/config/." "$WORK/payload/config/"
cp -f "$E2E_DIR/provision/provision.sh" "$E2E_DIR/provision/gen-pki.sh" "$WORK/payload/"

log "fetching Fedora cloud image…"
IMG="$WORK/base.qcow2"
curl -fSL -o "$IMG" "$FEDORA_IMG_URL"
if [ -n "$FEDORA_IMG_SHA256" ]; then echo "$FEDORA_IMG_SHA256  $IMG" | sha256sum -c - || die "checksum mismatch"; else
  warn "FEDORA_IMG_SHA256 unset — NOT reproducible; pin it."; fi

log "creating golden disk (resize to ${DISK_GB}G)…"
cp -f "$IMG" "$OUT"
qemu-img resize "$OUT" "${DISK_GB}G"

# bootstrap cloud-init: a build user with our ssh key + grow the rootfs
PUB="$(cat "${E2E_SSH_KEY}.pub")"
cat >"$WORK/user-data" <<EOF
#cloud-config
users: [ { name: build, sudo: "ALL=(ALL) NOPASSWD:ALL", ssh_authorized_keys: [ "$PUB" ] } ]
growpart: { mode: auto, devices: ['/'] }
EOF
printf 'instance-id: %s\nlocal-hostname: golden-build\n' "$BUILD_RUNID" >"$WORK/meta-data"
mkdir -p "$E2E_IMG_ROOT"; SEED_ISO="$E2E_IMG_ROOT/golden-build-seed.iso"   # pool path
iso_make "$SEED_ISO" "$WORK/user-data" "$WORK/meta-data"

log "booting build VM…"
virsh_ destroy "$BUILD_RUNID" 2>/dev/null || true; virsh_ undefine "$BUILD_RUNID" 2>/dev/null || true
virt-install --connect "$LIBVIRT_URI" --name "$BUILD_RUNID" --memory "$BUILD_MEM_MB" --vcpus 4 --import \
  --disk "path=$OUT,format=qcow2,bus=virtio" --disk "path=$SEED_ISO,device=cdrom" \
  --os-variant fedora-unknown --network network="$E2E_NET" --graphics none --noautoconsole --transient
IP="$(vm_ip "$BUILD_RUNID")" || die "build VM got no IP"
wait_tcp "$IP" 22 300 || die "build VM ssh never came up"
log "build VM up at $IP; provisioning (this is the slow part)…"

E2E_SSH_USER=build vm_ssh "$IP" 'mkdir -p ~/payload'
E2E_SSH_USER=build vm_scp "$WORK/payload/." "$IP" 'payload/'
E2E_SSH_USER=build vm_ssh "$IP" 'chmod +x ~/payload/provision.sh ~/payload/gen-pki.sh && PAYLOAD=$HOME/payload sudo -E ~/payload/provision.sh'

log "pulling CA cert out for host trust…"
scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -i "$E2E_SSH_KEY" "build@$IP:ca.crt" "${OUT%.qcow2}-ca.crt" 2>/dev/null \
  || warn "could not fetch ca.crt (check gen-pki output)"

log "clean shutdown + finalize golden…"
E2E_SSH_USER=build vm_ssh "$IP" 'sudo cloud-init clean --logs && sudo shutdown -h now' || true
for i in $(seq 1 60); do virsh_ domstate "$BUILD_RUNID" 2>/dev/null | grep -q 'shut off' && break; sleep 2; done
virsh_ destroy "$BUILD_RUNID" 2>/dev/null || true

log "GOLDEN READY: $OUT"
[ -f "${OUT%.qcow2}-ca.crt" ] && log "install CA in host trust: sudo cp '${OUT%.qcow2}-ca.crt' /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust"
log "then: GOLDEN_IMAGE='$OUT' scripts/e2e-vm/run.sh --profile fake"
