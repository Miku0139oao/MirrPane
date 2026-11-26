# 鏡屏 MirrPane

<img src="assets/logo.png" width="80" alt="">

**繁體中文** | [English](README.en.md)

**Windows AirPlay 螢幕鏡像接收器** — 基於 **Go + FFmpeg D3D11VA + MyGo** 打造，以原生桌面介面將 Windows 電腦變成 iPhone / iPad 的無線外接顯示器。提供免管理員的使用者安裝檔，以及整包解壓即用的便攜版；便攜版不寫入系統登錄檔、無需額外安裝執行期環境。

- **授權條款**：[GPL-3.0 License](https://github.com/Miku0139oao/MirrPane/blob/main/LICENSE)
- **專案儲存庫**：[Miku0139oao/MirrPane](https://github.com/Miku0139oao/MirrPane)
- **v0.4.4 正式版** · [Windows 安裝檔（建議）][setup] · [便攜完整版][win64] · [便攜精簡版][lite] · [版本說明][release] · [標籤原始碼][source-tag]

---

## 主要特色

- **硬體加速低延遲解碼**：透過 FFmpeg **D3D11VA** 進行 H.264 硬體加速解碼，採用直通式 Annex-B access unit 管線；不支援硬解的環境可退回 CPU 軟體解碼，也可在設定中指定軟解。
- **零 CGO 的 UI 與後端**：桌面介面（MyGo）與 `airplayd` 皆以 `CGO_ENABLED=0` 建置，透過 `purego` 在執行時動態綁定 FFmpeg，並由 Go 呼叫 Windows 系統 API，無需 Visual Studio C++、MinGW 或 C 開發標頭檔。
- **共享記憶體影格渲染**：後端將 RGBA 影格發布至每個 UI 實例專屬的 Windows 共享記憶體（`Local\MirrPaneFrames-<pid>`），透過 3-slot mailbox 與事件喚醒機制，由原生 MyGo UI（`internal/framefeed`）直接映射讀取並上傳紋理，省去跨程序影像序列化，也避免多個實例互搶同一區段；獨立執行 `airplayd` 時仍預設 `Local\YingpingFrames`。
- **直寫共享記憶體與閒置節流**：色彩轉換（swscale）直接寫入已取得的共享記憶體 slot，免去每幀一次整幀複製；當 UI 最小化或停止取幀超過一秒時，暫停下載、轉換與發佈（仍持續解碼參考幀），並以每秒一次的探測幀保留喚醒能力。
- **低延遲同步音訊**：解碼 AAC-ELD 音訊串流並透過 Windows 原生 `winmm waveOut` 驅動播放，支援即時音量調整、靜音與三檔延遲／抗抖動設定（低延遲、平衡、穩定），並以較大的環形緩衝吸收 Wi-Fi 爆量；環形緩衝會在高水位時平滑追上目標延遲，避免突發壅塞永久墊高延遲。
- **即時管線量測**：`/api/state` 提供 fps、解碼／GPU 下載／色彩轉換／發佈的均值與 p95、UI 端採用進度與音訊緩衝指標，便於定位延遲與掉幀。
- **自動化 FFmpeg 管理**：固定上游建置版本（含 SHA256 驗證與 ABI 檢查），首次啟動可自動偵測、續傳下載並安裝；DLL 只從明確的信任位置載入，不搜尋 `PATH` 或工作目錄。
- 鏡像與音訊串流使用核准裝置的配對身分，並綁定傳送端。本機管理 API 檢查 `Host` 與 `Origin`，防護細節見[安全性](#安全性)。
- **綠色免安裝**：便攜版的設定檔（`config.json`）、UI 偏好與配對金鑰皆儲存於程式資料夾，不寫入 Windows 登錄檔，也不需要系統管理員權限；安裝版安裝於 `%LOCALAPPDATA%\Programs\MirrPane`，僅新增標準的使用者層級解除安裝項目。唯一例外是 MyGo 於首次啟動在 `%LOCALAPPDATA%\MyGo\ShaderCache` 建立的著色器快取。
- **單一實例與精簡包**：同一登入工作階段只允許一個接收器，重複啟動會把既有視窗帶回前景，避免兩個實例搶用 AirPlay 連接埠並重複廣播名稱；另提供不含 FFmpeg 的精簡包（約 10 MB），首次啟動自動下載並以 SHA-256 驗證固定版 runtime。

---

## 系統架構

```text
iPhone / iPad (AirPlay / mDNS)
        │
        ▼
airplayd (Go Sidecar, CGO_ENABLED=0)
  ├── mDNS / Bonjour 廣播 (zeroconf)
  ├── FairPlay 金鑰交換與 AirPlay 配對 (pair-setup / pair-verify)
  ├── 視訊解碼：purego 綁定 FFmpeg D3D11VA (H.264 Annex-B)
  └── 音訊輸出：AAC-ELD → Windows winmm waveOut (低延遲／平衡／穩定)
        │
        ▼  視訊：3-slot mailbox 共享記憶體 (Local\MirrPaneFrames-<pid>) + Windows 事件喚醒
        │  狀態與控制：本機 HTTP API (IPC)
        ▼
MyGo 原生桌面 UI (backend/cmd/mirrpane, CGO_ENABLED=0)
  └── framefeed 共享記憶體讀取器 → ui.Bitmap 紋理
```

---

## 專案目錄結構

```text
MirrPane/
├── .github/workflows/          # CI 與 Release 自動化工作流程
├── backend/                    # Go 後端 (module: github.com/miku0139/airplay-go/backend)
│   ├── cmd/
│   │   ├── airplayd/           # 主後端 sidecar 與本機 HTTP API (main.go, sink.go, state.go, record.go 等)
│   │   ├── ffmpeg-setup/       # FFmpeg 共享 DLL 下載、驗證與安裝工具
│   │   └── mirrpane/           # 原生 MyGo 桌面 UI (main.go, app.go, sidecar.go, prefs.go 等)
│   ├── internal/
│   │   ├── audio/              # 音訊串流接收、解密與 AAC-ELD 解碼調度
│   │   ├── audioout/           # Windows winmm waveOut 音訊輸出與抗抖動緩衝
│   │   ├── config/             # config.json 讀寫管理
│   │   ├── decode/             # purego FFmpeg DLL 綁定、視訊與音訊解碼封裝
│   │   ├── fairplay/           # FairPlay 金鑰交換與解密實作
│   │   ├── ffmpeg/             # FFmpeg 執行環境偵測、下載、驗證與快取管理
│   │   ├── framefeed/          # UI 讀取共享記憶體影格的 reader
│   │   ├── frames/             # 影格傳遞與共享記憶體格式
│   │   ├── ipc/                # AirPlay RTSP 控制端點與封包解析
│   │   ├── mdns/               # AirPlay mDNS / Bonjour 廣播與裝置發現
│   │   ├── mirror/             # AirPlay 螢幕鏡像串流接收與 Annex-B 處理
│   │   ├── pairing/            # AirPlay pair-setup / pair-verify 配對協定
│   │   ├── protocol/           # AirPlay 裝置資訊、功能宣告與區網介面選擇
│   │   └── receiver/           # 接收器核心調度
│   ├── build.ps1               # 後端建置腳本
│   ├── go.mod
│   └── go.sum
├── assets/                     # 專案標誌與 Windows 圖示
├── scripts/
│   ├── fetch-ffmpeg.ps1        # 偵測或下載固定版本 Windows x64 FFmpeg shared build (SHA256)
│   ├── generate-app-icon.ps1   # 從 assets/logo.png 產生多尺寸 ICO 與執行檔圖示資源
│   ├── generate-ffmpeg-abi.ps1 # 產生 FFmpeg ABI 綁定相關定義
│   └── third-party-notices.ps1 # 收集隨附 Go 模組的授權文件
├── third_party/                # 第三方相依存放目錄 (如本地預載的 ffmpeg)
├── analysis/                   # 安全掃描報告與修補 patch（MirrPane/）
├── LICENSE                     # GPL-3.0 授權文件
├── README.md                   # 專案說明文件
├── README.en.md                # 英文版專案說明
├── VERSION                     # 發行版本來源（package.ps1 預設讀取）
└── package.ps1                 # 整包建置、打包與 SHA256 產生腳本
```

---

## 快速使用

1. 下載 [Windows 安裝檔][setup] 直接執行（免管理員，安裝到 `%LOCALAPPDATA%\Programs\MirrPane`，含開始功能表捷徑與解除安裝），或解壓[便攜完整版][win64]／[便攜精簡版][lite] 後執行 `MirrPane.exe`。驗證下載時，可將 `Get-FileHash` 算出的 SHA-256 與各檔旁 `.sha256` 校驗檔的值比對；精簡版首次啟動需連網下載並驗證 FFmpeg runtime。
2. 確認 Windows 電腦與 iPhone / iPad 位於同一區域網路，並在 Windows 防火牆提示中允許接收器存取私人網路。
3. 在 iPhone / iPad 開啟「控制中心」→ 點選「螢幕鏡像輸出」→ 選擇接收器名稱（預設為 `鏡屏`）。
4. 首次連線時，在電腦上確認裝置名稱並按「允許連線」，再回到 iPhone 或 iPad 選一次「鏡屏」。之後使用相同配對身分即可直接連線。
5. 介面提供 `F11` 全螢幕、`Ctrl+T` 置頂、`Ctrl+,`／`Ctrl+K` 設定、`Ctrl+L` 日誌與 `Ctrl+S` 截圖等快捷鍵；設定視窗包含一般、圖形與解碼、渲染、音訊等區段。

---

## 本機開發與建置

### 環境需求

- **Go**：`1.27.4+`（`go.mod` 會透過 `GOTOOLCHAIN=auto` 自動選用）
- 在 Windows x64 上使用 PowerShell 建置便攜包。MyGo 介面與接收器皆以 `CGO_ENABLED=0` 編譯。

### 建置步驟

1. **下載 FFmpeg 共享函式庫**（於專案根目錄執行，自動驗證 SHA256）：

   ```powershell
   .\scripts\fetch-ffmpeg.ps1 -Pinned
   ```

2. **建置與測試 Go UI 與後端**（從專案根目錄切換至 `backend/`，完成後返回）：

   ```powershell
   $env:CGO_ENABLED = '0'
   # 讓測試找到 repo 內的固定版 FFmpeg（Find 不再搜尋工作目錄/PATH）
   $env:MIRRPANE_FFMPEG_DIR = @(.\scripts\fetch-ffmpeg.ps1 -Pinned -PassThru)[-1]
   cd backend
   go build ./...
   go vet ./...
   go test ./...
   cd ..
   ```

   開發時可直接啟動 UI，並以 `-install-dir` 指向任一已打包的資料夾（需含 `backend\airplayd.exe`）：

   ```powershell
   cd backend
   $version = (Get-Content ..\VERSION -Raw).Trim()
   go run ./cmd/mirrpane -install-dir "..\dist\鏡屏-$version"
   ```

3. **整包建置與打包**（於專案根目錄執行）：

   ```powershell
   .\package.ps1
   ```

   版本預設讀取 `VERSION`；`-Version 0.0.0-test1` 可產生獨立測試版。輸出為 `dist\鏡屏-<version>\`（可直接執行）與 `dist\MirrPane-<version>-win64.zip`、`.sha256`。每次打包都在獨立的 `dist\.staging` 資料夾中重新建置（Go 建置快取會依內容跳過未變更的工作），不會覆寫共用的 `backend\*.exe`／`*.dll`；`-SkipBuild` 僅為相容舊命令列而接受，仍會執行快取建置。打包不會覆寫正在執行的資料夾；重新打包既有資料夾時，會保留其 `config\`、`logs\`、`screenshots\` 與其他未知使用者檔案（包含 `backend\` 內新增的檔案），並將舊安裝移至 `dist\.staging\previous-*` 備份。加上 `-Lite` 可另產不含 FFmpeg 的精簡包（`MirrPane-<version>-win64-lite.zip`，首次啟動自動下載 runtime）；`installer\build-installer.ps1 -Version <version>` 以 Inno Setup 6 產生 `MirrPane-<version>-setup.exe`（需要 `winget install --id JRSoftware.InnoSetup -e` 或 CI 的 Chocolatey 安裝）。

### 驗證與測試項目

- **Windows 建置驗證**：CI 以 `CGO_ENABLED=0` 建置並測試原生 UI 與後端，再執行 `package.ps1` 產生可攜壓縮檔與 SHA256。
- **跨平台協定測試**：於 Linux 環境執行 Go 協定與加密單元測試，包含 FairPlay 1024 組參考測試資料比對，並交叉編譯完整 Windows 應用程式。

---

## `airplayd` 後端 CLI 參數

`airplayd` 通常由原生 MyGo UI 作為 sidecar 自動啟動與管理，亦可獨立執行以進行測試或離線錄製除錯：

| 參數 | 預設值 | 說明 |
| :--- | :--- | :--- |
| `-name` | `"鏡屏"` | 設定檔首次建立時使用的 AirPlay 接收器名稱；之後以儲存的名稱為準 |
| `-data` | 執行檔旁的 `config` 目錄 | 設定檔（`config.json`）與配對金鑰儲存資料夾 |
| `-port` | `0`（優先嘗試 `7000`） | AirPlay 控制埠；設為 `0` 時優先使用 `7000` 否則自動挑選 |
| `-api-port` | `0`（自動挑選） | 提供給 UI 溝通的本機 HTTP API 埠 |
| `-width` | `0` | 虛擬顯示器寬度（設為 `0` 時讀取 `config.json`） |
| `-height` | `0` | 虛擬顯示器高度（設為 `0` 時讀取 `config.json`） |
| `-fps` | `0` | 虛擬顯示器最高幀率（設為 `0` 時讀取 `config.json`） |
| `-record` | 空字串 | 將接收到的原始 H.264 鏡像串流儲存至指定資料夾供離線重播除錯 |
| `-shm` | `Local\YingpingFrames` | 供 UI 讀取影格的 Windows 共享記憶體區段名稱；UI 啟動時會改傳每個實例專屬的 `Local\MirrPaneFrames-<pid>` |
| `-log` | 空字串 | 指定日誌檔案並啟用詳細記錄；未指定時仍會寫入預設的 `logs/airplayd.log` |
| `-parent-pid` | `0` | 父行程 PID（由 UI 傳入，當 UI 關閉時自動結束後端） |
| `-v` | `false` | 啟用詳細日誌輸出（verbose logging） |

### `MirrPane.exe` UI 參數

| 參數 | 預設值 | 說明 |
| :--- | :--- | :--- |
| `-install-dir` | 執行檔所在資料夾 | 覆寫安裝根目錄（需含 `backend\airplayd.exe`）；以 `go run ./cmd/mirrpane` 開發時必須指定 |
| `-shm` | `Local\MirrPaneFrames-<pid>` | 與 sidecar 共用的共享記憶體區段；UI 會把同一個名稱傳給子行程 |
| `-demo` | `false` | 顯示合成測試畫面，不啟動 sidecar（UI 開發用） |
| `-demo-settings` | `false` | 啟動時直接開啟設定視窗（UI 測試用） |

---

## FFmpeg 偵測順序與網路排錯

### FFmpeg 搜尋順序

後端只從明確的信任位置尋找固定版 FFmpeg 共享函式庫（`.dll`），**不會**搜尋 `PATH` 或目前工作目錄（避免 DLL 劫持）。若未找到，首次啟動會自動下載並驗證；也可透過 `backend/cmd/ffmpeg-setup` 或 `scripts/fetch-ffmpeg.ps1 -Pinned` 預先取得：

1. 環境變數 `MIRRPANE_FFMPEG_DIR` 指定路徑（權威來源；路徑錯誤會直接報錯，不會退回其他安裝）
2. 執行檔所在目錄、其下的 `ffmpeg/` 資料夾，以及該 `ffmpeg/` 內的已驗證快取
3. （開發用）以 `MIRRPANE_FFMPEG_DIR` 明確指向 repo 的 `third_party/ffmpeg/`；測試與 CI 皆採此方式

需使用相容的 Windows x64 shared build；僅有靜態連結的 `ffmpeg.exe` 無法提供所需 DLL。可攜版已附帶共享函式庫，可離線啟動。支援的 DLL 版本與 ABI 維護方式，請見 [第三方函式庫說明](third_party/README.md)。

### TUN / VPN 與裝置發現排錯

若 iPhone / iPad 找不到接收器，除了網路與防火牆設定，也可能與 TUN 模式或 VPN 虛擬網卡的 mDNS 廣播路由有關：

- MirrPane 會優先選擇具備 IPv4 Gateway 的 Ethernet / Wi-Fi 區網介面（包含 Hyper-V 外部交換器），並主動避開 TUN 介面、`198.18.0.0/15` 保留網段與 CGNAT 網段。
- 若仍無法發現裝置，請確認電腦與手機位於同一區網、Windows 防火牆允許 `airplayd` 與區域網路 mDNS（UDP 5353）通訊；亦可暫時停用 VPN 的 TUN 模式協助排錯。

---

## 安全性

接收器會核對傳送端的配對公鑰，核准名單存放在 `config\trusted_senders.json`。在設定中選擇「清除信任裝置」會中斷目前連線，裝置下次連線時需重新核准。[信任名單實作](backend/internal/trust/store.go)

Windows 透過 DPAPI 保護接收器的配對金鑰種子，並以檔案權限限制設定與信任名單的存取。本機管理 API 僅監聽 loopback。

---

## 技術依賴與致謝

### 主要技術依賴

- **UI**：`github.com/egoist/mygo`（原生 Windows 介面，含 `github.com/go-text/typesetting`、`golang.org/x/image` 等間接相依；隨附授權見發行版內的 `THIRD_PARTY_LICENSES.txt`）
- **後端**：`github.com/ebitengine/purego`、`github.com/grandcat/zeroconf`、`howett.net/plist`、`golang.org/x/sys`、`github.com/google/uuid`

### 致謝

本專案參考並受益於以下開源專案：

- [UxPlay](https://github.com/FDH2/UxPlay)
- [FFmpeg](https://ffmpeg.org/)
- [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)
- [MyGo](https://github.com/egoist/mygo)

[release]: https://github.com/Miku0139oao/MirrPane/releases/tag/v0.4.4
[win64]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.4/MirrPane-0.4.4-win64.zip
[checksum]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.4/MirrPane-0.4.4-win64.zip.sha256
[setup]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.4/MirrPane-0.4.4-setup.exe
[lite]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.4/MirrPane-0.4.4-win64-lite.zip
[source-tag]: https://github.com/Miku0139oao/MirrPane/tree/v0.4.3
