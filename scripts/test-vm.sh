#!/usr/bin/env bash
# A disposable Ubuntu 24.04 test server on this machine (QEMU/KVM, no root needed).
#
#   scripts/test-vm.sh create    download Ubuntu (once), create and boot the VM
#   scripts/test-vm.sh start     boot an existing VM
#   scripts/test-vm.sh ssh [cmd] open a shell (or run a command) in the VM
#   scripts/test-vm.sh copy F... copy files into the VM's home folder
#   scripts/test-vm.sh status    show whether it is running
#   scripts/test-vm.sh stop      shut it down (keeps the disk)
#   scripts/test-vm.sh destroy   stop and delete the VM disk (keeps the downloaded image)
#
# From this machine: SSH on 127.0.0.1:2222, the VM's HTTPS (port 443) on https://127.0.0.1:8443,
# and its agent port (7443) on 127.0.0.1:17443.
# Settings: VM_DIR (default ~/vms/panel-test), VM_RAM_MB (2048), VM_CPUS (2), VM_DISK (12G).
#
# A second VM (for name servers and other things between servers) needs its own folder,
# name and ports, and a private network to the first: start the first with VM_LINK=listen,
# then the second with VM_LINK=connect. The link is a second network card in each VM
# (address it with ip addr, such as 192.168.77.1/24 and 192.168.77.2/24):
#   VM_LINK=listen scripts/test-vm.sh start
#   VM_DIR=~/vms/panel-test2 VM_NAME=panel-test2 VM_PORT_OFFSET=10 VM_LINK=connect scripts/test-vm.sh create

set -euo pipefail

VM_DIR="${VM_DIR:-$HOME/vms/panel-test}"
NAME="${VM_NAME:-panel-test}"
RAM="${VM_RAM_MB:-2048}"
CPUS="${VM_CPUS:-2}"
DISK_SIZE="${VM_DISK:-12G}"
OFFSET="${VM_PORT_OFFSET:-0}"
SSH_PORT=$((2222 + OFFSET))
HTTPS_PORT=$((8443 + OFFSET))
AGENT_PORT=$((17443 + OFFSET))
SEED_PORT=$((8099 + OFFSET))
LINK_PORT=12399

IMAGE_URL="https://cloud-images.ubuntu.com/noble/current"
IMAGE_NAME="noble-server-cloudimg-amd64.img"
BASE="$VM_DIR/download/$IMAGE_NAME"
DISK="$VM_DIR/disk.qcow2"
KEY="$VM_DIR/id_ed25519"
PIDFILE="$VM_DIR/vm.pid"

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

running() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

ssh_vm() {
  ssh -q -i "$KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o LogLevel=ERROR ubuntu@127.0.0.1 "$@"
}

download() {
  mkdir -p "$VM_DIR/download"
  [ -s "$BASE" ] && return
  say "Downloading Ubuntu 24.04 cloud image"
  curl -fL --progress-bar -o "$BASE.part" "$IMAGE_URL/$IMAGE_NAME"
  curl -fsSL -o "$VM_DIR/download/SHA256SUMS" "$IMAGE_URL/SHA256SUMS"
  want="$(grep " \*\?$IMAGE_NAME\$" "$VM_DIR/download/SHA256SUMS" | awk '{print $1}')"
  got="$(sha256sum "$BASE.part" | awk '{print $1}')"
  [ -n "$want" ] && [ "$want" = "$got" ] || { rm -f "$BASE.part"; fail "checksum mismatch for the Ubuntu image"; }
  mv "$BASE.part" "$BASE"
}

boot() {
  running && { say "VM is already running"; return; }
  [ -w /dev/kvm ] || fail "/dev/kvm is not available; enable virtualization in the BIOS."
  # cloud-init reads its settings from a small HTTP server on the host (first boot only).
  local seed_pid=""
  if [ ! -f "$VM_DIR/.provisioned" ]; then
    python3 -m http.server "$SEED_PORT" --bind 127.0.0.1 --directory "$VM_DIR/seed" >/dev/null 2>&1 &
    seed_pid=$!
  fi
  local link=()
  case "${VM_LINK:-}" in
    "") ;;
    listen | connect)
      link=(-netdev "socket,id=link,$VM_LINK=127.0.0.1:$LINK_PORT" -device "virtio-net-pci,netdev=link,mac=52:54:00:77:00:$(printf '%02x' $((OFFSET + 1)))") ;;
    *) fail "VM_LINK is listen or connect" ;;
  esac
  say "Booting the VM ($CPUS CPUs, $RAM MB RAM)"
  qemu-system-x86_64 \
    -name "$NAME" -enable-kvm -cpu host -smp "$CPUS" -m "$RAM" \
    -drive "file=$DISK,if=virtio,discard=unmap" \
    -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22,hostfwd=tcp:127.0.0.1:$HTTPS_PORT-:443,hostfwd=tcp:127.0.0.1:$AGENT_PORT-:7443" \
    "${link[@]}" \
    -smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$SEED_PORT/" \
    -display none -serial "file:$VM_DIR/console.log" \
    -daemonize -pidfile "$PIDFILE"

  say "Waiting for SSH"
  for _ in $(seq 1 120); do
    if ssh_vm true 2>/dev/null; then
      [ -n "$seed_pid" ] && { ssh_vm cloud-init status --wait >/dev/null 2>&1 || true; kill "$seed_pid" 2>/dev/null || true; touch "$VM_DIR/.provisioned"; }
      say "VM is ready. SSH: scripts/test-vm.sh ssh   HTTPS: https://127.0.0.1:$HTTPS_PORT"
      return
    fi
    sleep 2
  done
  [ -n "$seed_pid" ] && kill "$seed_pid" 2>/dev/null
  fail "the VM did not become reachable; see $VM_DIR/console.log"
}

create() {
  [ -f "$DISK" ] && fail "a VM already exists in $VM_DIR (use start, or destroy first)"
  command -v qemu-system-x86_64 >/dev/null || fail "install QEMU first (qemu-system-x86)"
  download
  say "Creating the VM disk ($DISK_SIZE)"
  qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$DISK" "$DISK_SIZE"
  [ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N "" -C "$NAME" -f "$KEY"
  mkdir -p "$VM_DIR/seed"
  printf 'instance-id: %s-%s\nlocal-hostname: %s\n' "$NAME" "$(date +%s)" "$NAME" > "$VM_DIR/seed/meta-data"
  cat > "$VM_DIR/seed/user-data" <<EOF
#cloud-config
ssh_authorized_keys:
  - $(cat "$KEY.pub")
ssh_pwauth: false
package_update: false
EOF
  : > "$VM_DIR/seed/vendor-data"
  rm -f "$VM_DIR/.provisioned"
  boot
}

stop() {
  if ! running; then say "VM is not running"; return; fi
  say "Shutting down the VM"
  ssh_vm sudo poweroff >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do running || break; sleep 1; done
  running && kill "$(cat "$PIDFILE")"
  rm -f "$PIDFILE"
  say "Stopped"
}

case "${1:-}" in
  create) create ;;
  start) [ -f "$DISK" ] || fail "no VM yet; run: $0 create"; boot ;;
  ssh) shift; ssh_vm "$@" ;;
  copy) shift; scp -q -i "$KEY" -P "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$@" ubuntu@127.0.0.1: ;;
  status) running && echo "running (pid $(cat "$PIDFILE"))" || echo "stopped" ;;
  stop) stop ;;
  destroy) stop; rm -rf "$DISK" "$VM_DIR/seed" "$VM_DIR/.provisioned" "$VM_DIR/console.log"; say "VM deleted (image kept in $VM_DIR/download)" ;;
  *) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
