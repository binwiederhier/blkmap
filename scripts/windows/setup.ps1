# First logon of a golden image (see autounattend.xml): remote management, no updates, no
# sleep, SQL Server Developer (sql-install.ps1), then shut down: golden.sh waits for the guest
# to exit.
$ErrorActionPreference = "Continue"
$media = (Get-Volume -FileSystemLabel UNATTEND).DriveLetter + ":"

# Remote management over WinRM (lab only: basic auth, unencrypted, behind QEMU NAT)
Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private
# The build runs without a network: make the unidentified network the soak attaches later
# private too, or client editions keep WinRM behind the public profile
$nl = "HKLM:\SOFTWARE\Policies\Microsoft\Windows NT\CurrentVersion\NetworkList\Signatures\010103000F0000F0010000000F0000F0C967A3643C3AD745950DA7859209176EF5B87C875FA20DF21951640E807D7C24"
New-Item $nl -Force | Out-Null
Set-ItemProperty $nl Category 1 -Type DWord
# Enable-PSRemoting, not winrm quickconfig: client editions refuse quickconfig on a public or
# missing network (the build runs without one), which left Basic auth off on Windows 11
Enable-PSRemoting -SkipNetworkProfileCheck -Force
# Set-Item WSMan:\... refuses while the network is public (it is during the build), so write
# what it would write and restart the service
$ws = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WSMAN\Service"
Set-ItemProperty $ws allow_unencrypted 1 -Type DWord
Set-ItemProperty $ws auth_basic 1 -Type DWord
Restart-Service WinRM
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
# The soak serves the disk over VirtIO SCSI (see win-vm.sh and vioscsi.ps1)
& "$media\vioscsi.ps1"
# No BitLocker: a soak guest boots with a fresh TPM (see autounattend.xml)
Disable-BitLocker -MountPoint C: -ErrorAction SilentlyContinue
while ((Get-BitLockerVolume -MountPoint C: -ErrorAction SilentlyContinue).VolumeStatus -eq "DecryptionInProgress") { Start-Sleep 10 }
# A crash must leave a guest that comes back by itself
Set-ItemProperty HKLM:\SYSTEM\CurrentControlSet\Control\CrashControl AutoReboot 1 -Type DWord
bcdedit /set "{current}" bootstatuspolicy ignoreallfailures

# SQL Server Developer from the ISO golden.sh attaches (the web bootstrappers go stale)
& "$media\sql-install.ps1"

Set-Content C:\golden-done.txt (Get-Date -Format o)
Stop-Computer -Force
