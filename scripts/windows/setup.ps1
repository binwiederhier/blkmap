# First logon of a golden image (see autounattend.xml): remote management, no updates, no
# sleep, SQL Server Developer (sql-install.ps1), then shut down: golden.sh waits for the guest
# to exit.
$ErrorActionPreference = "Continue"
$media = (Get-Volume -FileSystemLabel UNATTEND).DriveLetter + ":"

# Remote management over WinRM (lab only: basic auth, unencrypted, behind QEMU NAT)
Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private
winrm quickconfig -q -force
winrm set winrm/config/service '@{AllowUnencrypted="true"}'
winrm set winrm/config/service/auth '@{Basic="true"}'
Set-ItemProperty HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System LocalAccountTokenFilterPolicy 1 -Type DWord
New-NetFirewallRule -DisplayName "WinRM 5985" -Direction Inbound -Protocol TCP -LocalPort 5985 -Action Allow
New-NetFirewallRule -DisplayName "SQL Server 1433" -Direction Inbound -Protocol TCP -LocalPort 1433 -Action Allow

# Nothing that changes the disk behind the test's back
Stop-Service wuauserv -Force; Set-Service wuauserv -StartupType Disabled
New-Item HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU -Force | Out-Null
Set-ItemProperty HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU NoAutoUpdate 1 -Type DWord
Set-MpPreference -DisableRealtimeMonitoring $true
powercfg /hibernate off
powercfg /change standby-timeout-ac 0
powercfg /change monitor-timeout-ac 0
# The soak serves the disk over NVMe (see win-vm.sh); its driver must start at boot
Set-ItemProperty HKLM:\SYSTEM\CurrentControlSet\Services\stornvme Start 0 -Type DWord
# A crash must leave a guest that comes back by itself
Set-ItemProperty HKLM:\SYSTEM\CurrentControlSet\Control\CrashControl AutoReboot 1 -Type DWord
bcdedit /set "{current}" bootstatuspolicy ignoreallfailures

# SQL Server Developer from the ISO golden.sh attaches (the web bootstrappers go stale)
& "$media\sql-install.ps1"

Set-Content C:\golden-done.txt (Get-Date -Format o)
Stop-Computer -Force
