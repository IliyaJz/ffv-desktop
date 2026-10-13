# FFV

FFV is a lightweight, high-performance terminal client for Firefox VPN supporting **macOS**, **Linux**, and **Windows**.

> Based on and inspired by [FoxyVPN](https://github.com/Vauth/FoxyVPN).

---

## How to Use

### 1. Sign In (Firefox Account)

```bash
# macOS:
./bin/ffv login

# Linux:
./bin/ffv-linux-amd64 login

# Windows:
.\bin\ffv.exe login
```

If direct access to Mozilla is restricted in your network, you can route the login through an upstream proxy:
```bash
./bin/ffv login --upstream-proxy socks5://127.0.0.1:1080
```

---

### 2. Interactive Terminal UI (TUI)

Launch the interactive dashboard:

```bash
# Full System-Wide TUN mode (requires root / admin):
sudo ./bin/ffv              # macOS
sudo ./bin/ffv-linux-amd64  # Linux
sudo .\bin\ffv.exe          # Windows (PowerShell with sudo or Run as Administrator)

# Proxy-Only mode (no root / admin needed):
./bin/ffv                   # macOS
./bin/ffv-linux-amd64       # Linux
.\bin\ffv.exe --proxy-only  # Windows
```

#### TUI Shortcuts
| Key | Action |
|---|---|
| <kbd>Space</kbd> | Connect / Disconnect tunnel |
| <kbd>m</kbd> | Toggle between TUN and Proxy-Only mode |
| <kbd>Tab</kbd> / <kbd>1</kbd>, <kbd>2</kbd>, <kbd>3</kbd> | Switch Tabs (Dashboard, Locations, Logs) |
| <kbd>/</kbd> | Search locations |
| <kbd>↑</kbd> / <kbd>↓</kbd> | Navigate locations |
| <kbd>Enter</kbd> | Select location & connect |
| <kbd>r</kbd> | Refresh server list and quota |
| <kbd>q</kbd> or <kbd>Ctrl+C</kbd> | Clean exit |

---

### 3. Command Line (CLI)

#### System-Wide TUN Mode
```bash
# macOS:
sudo ./bin/ffv connect --country FR

# Linux:
sudo ./bin/ffv-linux-amd64 connect --country FR

# Windows (Administrator PowerShell):
sudo .\bin\ffv.exe connect --country FR
```

#### Proxy-Only Mode (Local SOCKS5 on 127.0.0.1:1088, No Root/Admin)
```bash
# macOS:
./bin/ffv connect --country FR --proxy-only

# Linux:
./bin/ffv-linux-amd64 connect --country FR --proxy-only

# Windows:
.\bin\ffv.exe connect --country FR --proxy-only
```

#### Upstream Proxy Chaining
```bash
sudo ./bin/ffv connect --country US --upstream-proxy socks5://127.0.0.1:1080
```

#### Check Status / Quota
```bash
./bin/ffv status
```

#### List Available Locations
```bash
./bin/ffv servers
```
