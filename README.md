# ASRockRack IPMI Fan Control

Some ASRockRack motherboards include BMC functionality to intelligently control the fans based on temperature sensor values and either "open loop" tables, or a "closed loop" set of parameters. However, I could never get this to work at all, instead relying on fixed fan settings.

This Go program implements a PID logic for the fan speeds based on temperature sensors. It uses IPMI commands on the host to adjust fan speed. It has built-in defaults that can be customized through a config file.

## Sensors

The tool uses three classes of temperature sensors:
* IPMI sensors (`ipmitool sdr type Temperature`)
* NVMe SMART temperature attribute #2/1 (`nvme smart-log /dev/nvme0 | grep -i '^temperature'`)
* Hard Disk temperature attribute #194 (`smartctl -a /dev/sda | grep -i '^194'`)

For each sensor, you can configure the PID parameters. The tools tries to apply sensible defaults (see below) if no configuration exists for a sensor.

### Default PID Parameters

For all sensors, the target temperature is 35C, with a maximum of 70C that should not be exceeded. For CPUs, the target is 50C, with 90C not to be exceeded.

## Fans

Unless configured to be controlled by a specific set of sensors, all fans are controlled by all sensors PID loops. Whichever PID demands the most cooling determines the duty cycle.

Fan duty cycles are set with ASRock Rack OEM raw IPMI commands, sent directly over the local `/dev/ipmi0` OpenIPMI device. Temperature sensors are read the same way: IPMI sensors via the SDR repository (Reserve/Get SDR + Get Sensor Reading), NVMe/disk temperatures via the kernel's NVMe admin passthrough and ATA SMART ioctls. No `ipmitool`, `nvme`, or `smartctl` binaries are invoked.

The exact raw fan control command is firmware-specific and undocumented by ASRock Rack. On an X570D4U running BMC firmware 1.35 (product ID 0x1005), the `raw 0x3a 0x01 <8 bytes>` command described in various community write-ups for older/sibling boards does not exist (`0xc1`, "invalid command", confirmed with both this tool and `ipmitool` directly). The general shape of the protocol actually used here — which commands exist, roughly what they do — comes from [x570d4ufans-plugin](https://github.com/pjbulls/x570d4ufans-plugin), which reverse-engineered it for this exact board on different firmware; the exact payload lengths, value range, and rate limit below were independently confirmed against real X570D4U hardware on firmware 1.35, since that plugin's assumptions didn't fully match this firmware revision:

* `raw 0x3a 0xd8 <16 bytes, all 0x01>` — switch every fan zone to manual (host-controlled) duty; all `0x00` switches back to BMC automatic control.
* `raw 0x3a 0xd6 <16 bytes>` — set the duty cycle for the board's 6 real fan zones (FAN1, FAN2, FAN3, FAN4_1, FAN5_1, FAN6_1), followed by 10 padding bytes for header slots the board doesn't expose. Each duty byte is on a 15-63 raw scale, not a 0-100 percentage: `raw = 15 + (percent-20)*48/80`, percent clamped to [20,100]. Values of 0 or 64+ are rejected outright.
* The BMC also rejects large single-step duty changes (e.g. jumping from raw 63 straight to 40 in one command). This tool rate-limits each byte to a change of at most 8 raw units per call, seeded from a read of the BMC's current state (`raw 0x3a 0xda`), and lets duty converge over successive control loop ticks rather than jumping straight to target.

If you're on different BMC firmware and this doesn't work, run `ipmitool raw 0x3a 0xda` (read-only) to check the command is at least recognized, and please open an issue with your `ipmitool mc info` output.

## Build

Requires Go and a Linux target (the tool talks to `/dev/ipmi0` and NVMe/ATA ioctls directly, so it only builds/runs on Linux):

```
GOOS=linux go build -o asrockrack-ipmi-fan-control .
```

## Usage

```
sudo ./asrockrack-ipmi-fan-control [-config config.yaml] [-dry-run] [-once] [-verbose]
```

- `-config` — path to a YAML config file (see [config.example.yaml](config.example.yaml)); omit to run with built-in defaults only.
- `-dry-run` — compute fan duty cycles and log them, but don't send IPMI commands.
- `-once` — run a single control loop iteration and exit, useful for testing.
- `-verbose` — log each sensor reading and PID output, not just the resulting fan duty.

Root (or equivalent access to `/dev/ipmi0` and the NVMe/disk block devices) is required.

## Running as a service

A systemd unit is provided at [systemd/asrockrack-ipmi-fan-control.service](systemd/asrockrack-ipmi-fan-control.service). It runs the binary from `/usr/sbin/asrockrack-ipmi-fan-control` with `-config /etc/asrockrack-ipmi-fan-control/config.yaml`, restarting on failure. It is not enabled automatically by the Debian package (see below) — review your config first, then:

```
systemctl enable --now asrockrack-ipmi-fan-control
```

## Releases

Pushing a `vX.Y.Z` tag triggers [.github/workflows/release.yml](.github/workflows/release.yml), which builds a `linux/amd64` binary, packages it as a `.deb` (binary, systemd unit, and example config as `/etc/asrockrack-ipmi-fan-control/config.yaml`) using [nfpm](https://nfpm.goreleaser.com), and publishes a GitHub release for the tag with notes generated from the commits since the previous release. The nfpm package spec lives in [packaging/nfpm.yaml](packaging/nfpm.yaml).
