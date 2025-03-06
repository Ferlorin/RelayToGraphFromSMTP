# SMTP Relay To Microsoft Graph Middleware - Proxy

## Overview

This middleware allows applications using legacy Basic Authentication to send emails via [Microsoft Graph API](https://learn.microsoft.com/en-us/graph/use-the-api).

It includes support for **BCC recipients** and consolidates emails sent to different recipient types (**To**, **CC**, and **BCC**) to ensure the email is processed and sent only when the `QUIT` command is issued.

---

## Configuration

To configure the application, you need a `config.ini` file in the same folder as the app. Below is an example template:

```ini
[MicrosoftGraph]
TenantID = <YourTenantId>
ClientID = <YourClientId>
ClientSecret = <YourClientSecret>
Scope = https://graph.microsoft.com/.default

[Server]
SMTPPort = 2525
Host = 127.0.0.1
```

### Example `config.ini` File

```ini
[MicrosoftGraph]
TenantID = abc123.onmicrosoft.com
ClientID = 11111111-2222-3333-4444-555555555555
ClientSecret = supersecretkey
Scope = https://graph.microsoft.com/.default

[Server]
SMTPPort = 2525
Host = 127.0.0.1
```

- The default SMTP port is `2525`.
- Ensure the `TenantID`, `ClientID`, and `ClientSecret` are properly set for your Microsoft Graph configuration.

---

## Local Testing & Deployment

### **Windows**

#### 1. **Debug/Run Locally**

Simply build and run the application:

```bash
go build -o smtpservice.exe
./smtpservice.exe
```

The application will run as a standalone app.

---

#### 2. **Install as a Windows Service**

You can install the application as a Windows service either using the built-in installer logic or manually using `sc.exe`.

#### **Automatic Installation:**
Run the following command:

```cmd
smtpservice.exe install <serviceName> <displayName> <description>
```

Example:
```cmd
smtpservice.exe install MySMTPService "SMTP Relay Server" "Relays emails through Graph API"
```

This registers the service as `MySMTPService` in the Windows Services Manager with the specified display name and description.

#### **Manual Installation (Using sc.exe):**
Alternatively, to manually create the service:
1. Open the **Command Prompt (Admin)**.
2. Run the following command:
   ```cmd
   sc create MySMTPService binPath="C:\path\to\smtpservice.exe" DisplayName="SMTP Relay Server"
   ```
3. Start the service:
   ```cmd
   sc start MySMTPService
   ```

You can also manage the service using the **Services Manager GUI**:
- Press `Win + R`, type `services.msc`, and press Enter.
- Locate your service (`SMTP Relay Server`), then choose **Start**, **Stop**, or configure it.

---

#### 3. **Start/Stop the Service**

Use the `Service Control Manager` or run the following commands:

Start the service:
```cmd
sc start MySMTPService
```

Stop the service:
```cmd
sc stop MySMTPService
```

---

#### 4. **Remove the Service**

You can remove the service using the application's built-in uninstall logic or `sc.exe`.

**Using the Application:**
```cmd
smtpservice.exe remove MySMTPService
```

**Using sc.exe:**
```cmd
sc delete MySMTPService
```

---

### **Linux**

#### **Use Systemd for Linux Service Management**

---

#### Step 1: Place the Binary

Transfer your compiled binary to `/usr/local/bin`:

```bash
sudo mv smtpservice /usr/local/bin/
```

Make sure the binary is executable:
```bash
sudo chmod +x /usr/local/bin/smtpservice
```

---

#### Step 2: Create a Systemd Unit File

Create a service file for `systemd` in `/etc/systemd/system/smtpservice.service`:

```bash
sudo nano /etc/systemd/system/smtpservice.service
```

Add the following content:

```ini
[Unit]
Description=SMTP Relay Server
After=network.target

[Service]
ExecStart=/usr/local/bin/smtpservice
Restart=always
RestartSec=5
User=nobody                # Change this to the appropriate user
Group=nogroup              # Change this to the appropriate group
WorkingDirectory=/usr/local/bin/

[Install]
WantedBy=multi-user.target
```

> **Notes:**
- Replace `User` and `Group` with the appropriate user/group if required (e.g., `smtp` user).
- `ExecStart` must point to the absolute path of your binary.

---

#### Step 3: Reload and Start the Service

1. Reload the systemd daemon to recognize the new unit file:
   ```bash
   sudo systemctl daemon-reload
   ```

2. Enable the service to start at boot:
   ```bash
   sudo systemctl enable smtpservice
   ```

3. Start the service:
   ```bash
   sudo systemctl start smtpservice
   ```

4. Check the status of the service:
   ```bash
   sudo systemctl status smtpservice
   ```

---

#### Step 4: Logs

To view logs for the service, use:
```bash
sudo journalctl -u smtpservice
```

---

#### Step 5: Remove the Service

To uninstall the service:

1. Stop the service:
   ```bash
   sudo systemctl stop smtpservice
   ```

2. Disable it:
   ```bash
   sudo systemctl disable smtpservice
   ```

3. Remove the systemd unit file:
   ```bash
   sudo rm /etc/systemd/system/smtpservice.service
   ```

4. Reload systemd:
   ```bash
   sudo systemctl daemon-reload
   ```

---

## **Cross-Compile**

To cross-compile the application for Linux from a Windows development machine, use one of the following methods:

### For Linux (x86-64 architecture):

#### 1. **Command Prompt (cmd):**
```cmd
set GOOS=linux
set GOARCH=amd64
go build -o smtpservice
```

#### 2. **PowerShell:**
```powershell
$env:GOOS="linux"
$env:GOARCH="amd64"
go build -o smtpservice
```

#### 3. **Git Bash or WSL:**
```bash
GOOS=linux GOARCH=amd64 go build -o smtpservice
```

#### For ARM Architecture:
If targeting ARM (e.g., Raspberry Pi):
```bash
GOOS=linux GOARCH=arm GOARM=7 go build -o smtpservice
```

---

## Troubleshooting

### Common Issues

**1. Permission Denied (Linux or Windows)**:
- **Linux:** Run commands with `sudo` to avoid permission issues when managing services.
- **Windows:** Ensure the application is launched as an **Administrator**.

**2. Service Fails to Start**:
- Check the application logs (`journalctl -u smtpservice` on Linux or Windows Event Viewer).
- Verify that `config.ini` exists and is configured correctly.

**3. Missing `config.ini` File**:
- Ensure `config.ini` is in the same directory as the application.

---

## Contributing

We welcome contributions to this project! Please refer to the LICENSE file for more details on how you can contribute.

---

## License

This project is licensed under the terms of the MIT License. See the LICENSE file for more details.

---

## Support

For support and feature requests, please open an issue in the repository's issue tracker.