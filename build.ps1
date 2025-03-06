Write-Host "Building Linux:"
$env:GOOS="linux"
$env:GOARCH="amd64"
go build -o smtpservice

Write-Host "Building Windows x64:"
$env:GOOS="windows"
$env:GOARCH="amd64"
go build -o smtpservice_x64.exe

Write-Host "Building Windows x86:"
$env:GOARCH="386"
go build -o smtpservice_x86.exe

Write-Host "Builds complete: smtpservice smtpservice_x64.exe and smtpservice_x86.exe"