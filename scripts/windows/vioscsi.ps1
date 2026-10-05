# Installs the VirtIO SCSI driver (vioscsi) from an attached virtio-win ISO, for this Windows
# version. The soak serves the disk over VirtIO SCSI, the controller whose emulation turns a
# guest's write-through (FUA) writes into flushes; QEMU's AHCI and NVMe do not. A probe disk
# on VirtIO SCSI must be attached (win-vm.sh SCSI_PROBE=1) so Windows registers the driver
# as a boot driver.
$ErrorActionPreference = "Stop"
$cd = Get-Volume | Where-Object { $_.FileSystemLabel -like "virtio-win*" } | Select-Object -First 1
if (-not $cd) { throw "no virtio-win ISO attached" }
$os = Get-CimInstance Win32_OperatingSystem
$build = [int]$os.BuildNumber
$server = $os.ProductType -ne 1
$dir = if ($server) {
  if ($build -ge 26100) { "2k25" } elseif ($build -ge 20348) { "2k22" } elseif ($build -ge 17763) { "2k19" } else { "2k16" }
} else {
  if ($build -ge 22000) { "w11" } else { "w10" }
}
pnputil /add-driver "$($cd.DriveLetter):\vioscsi\$dir\amd64\vioscsi.inf" /install
# Wait until the probe disk's controller runs on it: then the service starts at boot
for ($i = 0; $i -lt 60 -and (Get-Service vioscsi -ErrorAction SilentlyContinue).Status -ne "Running"; $i++) { Start-Sleep 5 }
"vioscsi ($dir): $((Get-Service vioscsi).Status), start $((Get-Service vioscsi).StartType)"
