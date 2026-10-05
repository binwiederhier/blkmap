# Installs the SQL Server engine from an attached SQL Server ISO (volume label SQLServer*),
# default instance, SQL auth, TCP on 1433, started automatically. Lab credentials.
$ErrorActionPreference = "Stop"
$cd = Get-Volume | Where-Object { $_.FileSystemLabel -like "SQLServer*" } | Select-Object -First 1
if (-not $cd) { throw "no SQL Server ISO attached" }
$setup = $cd.DriveLetter + ":\setup.exe"
$p = Start-Process $setup -Wait -PassThru -ArgumentList @(
  "/Q", "/ACTION=Install", "/FEATURES=SQLENGINE", "/INSTANCENAME=MSSQLSERVER",
  '/SQLSYSADMINACCOUNTS="BUILTIN\Administrators"', "/SECURITYMODE=SQL", '/SAPWD="Blkmap-Lab-2026!"',
  "/TCPENABLED=1", "/UPDATEENABLED=0", "/IACCEPTSQLSERVERLICENSETERMS")
if ($p.ExitCode -ne 0) { throw "SQL Server setup exited with $($p.ExitCode); see C:\Program Files\Microsoft SQL Server\*\Setup Bootstrap\Log" }
Set-Service MSSQLSERVER -StartupType Automatic
Start-Service MSSQLSERVER
"SQL Server installed from $($cd.FileSystemLabel)"
