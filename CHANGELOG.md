# Changelog

## 1.0.2

### Fixed

- **Fans stuck at 100%**: sensors without a config entry whose name contains `X570`, `B550`, `PCH` or `Chipset` now default to target 55C / max 80C. Previously the X570 chipset (idling around 40C) was held against the general 35C target, which saturated its PID loop and held every fan at 100%.
- **PID anti-windup**: the integral term is clamped to the output range, and integration is only skipped while the output is saturated in the direction of the error, so the integral always unwinds once the temperature drops below target.
- **Duty ramp**: only the real fan zones (`fan_count`) are rate-limited; padding slots always get the pass-through value. Previously garbage in the padding slots of the seed read produced payloads the BMC rejected, on every tick, forever.
- **Seed validation and recovery**: the seed read is retried if it is short or out of range, falling back to the target duty. After a failed write the current duty is read back. After 12 consecutive failed writes the daemon exits non-zero so systemd restarts it.
- **Packaging**: the Debian package now enables and starts the service on install, restarts it on upgrade, and stops it on removal (`deb-systemd-helper`, as `dh_installsystemd` does). It now depends on `init-system-helpers`.

### Changed

- The per-tick `fan duty` INFO line names the sensor that determined each zone's duty, with its temperature and target, e.g. `driver="X570 Temp" temp=41 target=35`.
- README: documented the default sensor classes and corrected the raw duty scale (30-63, not 15-63).

## 1.0.1

- Releases also attach the plain `linux/amd64` binary alongside the `.deb`.

## 1.0.0

- Initial release.
