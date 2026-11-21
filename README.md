# 鏡屏 MirrPane

**繁體中文** | [English](README.en.md)

**Windows AirPlay 螢幕鏡像接收器** — 基於 **Go + FFmpeg D3D11VA + Flutter** 打造，將 Windows 電腦變成 iPhone / iPad 的無線外接顯示器。主打整包解壓即用、不寫入系統登錄檔、無需額外安裝執行期環境。

- **授權條款**：[GPL-3.0 License](https://github.com/Miku0139oao/MirrPane/blob/main/LICENSE)
- **專案儲存庫**：[Miku0139oao/MirrPane](https://github.com/Miku0139oao/MirrPane)
- **最新版本**：`v0.3.1`

---

## 主要特色

- **硬體加速低延遲解碼**：透過 FFmpeg **D3D11VA** 進行 H.264 硬體加速解碼，採用直通式 Annex-B access unit 管線；不支援硬解的環境可退回 CPU 軟體解碼，也可在設定中指定軟解。
- **零 CGO Go 後端**：以 `CGO_ENABLED=0` 建置，透過 `purego` 在執行時動態綁定 FFmpeg，並由 Go 呼叫 Windows 系統 API，無需 MinGW 或 C 開發標頭檔。
- **共享記憶體影格渲染**：後端將 RGBA 影格發布至 Windows 共享記憶體（`Local\YingpingFrames`），透過 3-slot mailbox 與事件喚醒機制，由原生 C++ 橋接（`yingping_frames`）提供 Flutter 紋理，省去跨程序影像序列化。
- **低延遲同步音訊**：解碼 AAC-ELD 音訊串流並透過 Windows 原生 `winmm waveOut` 驅動播放，支援即時音量調整、靜音與三檔抗抖動緩衝（超低延遲 `30 ms` / 標準平衡 `40 ms` / 弱網穩定 `60 ms`）。
- **自動化 FFmpeg 管理**：支援環境自動偵測與 SHA256 驗證下載，開箱即用。
- **綠色免安裝**：設定檔（`config.json`）與配對金鑰皆儲存於程式資料夾，不寫入 Windows 登錄檔。

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
  └── 音訊輸出：AAC-ELD → Windows winmm waveOut (30 / 40 / 60 ms 緩衝)
        │
        ▼  視訊：3-slot mailbox 共享記憶體 (Local\YingpingFrames) + Windows 事件喚醒
        │  狀態與控制：本機 HTTP API (IPC)
        ▼
Flutter Desktop UI (yingping)
  └── yingping_frames (C++ Windows Texture 橋接)
```

---

## 專案目錄結構

```text
MirrPane/
├── .github/workflows/          # CI 與 Release 自動化工作流程
├── backend/                    # Go 後端 (module: github.com/miku0139/airplay-go/backend)
│   ├── cmd/
│   │   ├── airplayd/           # 主後端 sidecar 與本機 HTTP API (main.go, sink.go, state.go, record.go 等)
│   │   └── ffmpeg-setup/       # FFmpeg 共享 DLL 下載、驗證與安裝工具
│   ├── internal/
│   │   ├── audio/              # 音訊串流接收、解密與 AAC-ELD 解碼調度
│   │   ├── audioout/           # Windows winmm waveOut 音訊輸出與抗抖動緩衝
│   │   ├── config/             # config.json 讀寫管理
│   │   ├── decode/             # purego FFmpeg DLL 綁定、視訊與音訊解碼封裝
│   │   ├── fairplay/           # FairPlay 金鑰交換與解密實作
│   │   ├── ffmpeg/             # FFmpeg 執行環境偵測、下載、驗證與快取管理
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
├── frontend/                   # Flutter 桌面前端 (yingping)
│   ├── assets/                 # 靜態資源 (logo.png 等)
│   ├── lib/
│   │   ├── main.dart           # UI 主程式、視窗與 4 分頁設定介面
│   │   ├── receiver_api.dart   # 呼叫後端本機 HTTP API 的用戶端
│   │   └── ui_prefs.dart       # 前端介面偏好設定
│   ├── packages/
│   │   └── yingping_frames/    # 本地 Flutter 插件 (C++ Windows 紋理與共享記憶體橋接)
│   ├── windows/                # Flutter Windows 平台原生工程與 CMake 設定
│   └── pubspec.yaml
├── scripts/
│   ├── fetch-ffmpeg.ps1        # 偵測或下載最新版 Windows x64 FFmpeg shared build (SHA256)
│   └── generate-ffmpeg-abi.ps1 # 產生 FFmpeg ABI 綁定相關定義
├── third_party/                # 第三方相依存放目錄 (如本地預載的 ffmpeg)
├── LICENSE                     # GPL-3.0 授權文件
├── README.md                   # 專案說明文件
├── README.en.md                # 英文版專案說明
└── package.ps1                 # 整包建置與打包 PowerShell 腳本
```

---

## 快速使用

1. 從 [Releases](https://github.com/Miku0139oao/MirrPane/releases) 下載最新版本（如 `v0.3.1`），解壓縮後直接執行 `MirrPane.exe`（無需安裝或寫入系統登錄檔）。
2. 確認 Windows 電腦與 iPhone / iPad 位於同一區域網路，並在 Windows 防火牆提示中允許接收器存取私人網路。
3. 在 iPhone / iPad 開啟「控制中心」→ 點選「螢幕鏡像輸出」→ 選擇接收器名稱（預設為 `鏡屏`）。
4. 雙擊視窗或按下 `F11` 可切換全螢幕；介面內建進階設定分頁（一般設定、連線資訊、即時統計、配對重設等）與快捷鍵操作。

---

## 本機開發與建置

### 環境需求

- **Go**：`1.26+`
- **Flutter**：`3.47+`（Dart SDK `^3.13.5`）
- **建置工具**：Visual Studio C++ Build Tools、CMake、PowerShell

### 建置步驟

1. **下載 FFmpeg 共享函式庫**（於專案根目錄執行，自動驗證 SHA256）：

   ```powershell
   .\scripts\fetch-ffmpeg.ps1
   ```

2. **建置與測試 Go 後端**（從專案根目錄切換至 `backend/`，完成後返回）：

   ```powershell
   $env:CGO_ENABLED = '0'
   cd backend
   go build ./...
   go test ./...
   cd ..
   ```

3. **整包建置與打包**（於專案根目錄執行）：

   ```powershell
   .\package.ps1
   ```

### 驗證與測試項目

- **Windows 建置驗證**：確保後端於 `CGO_ENABLED=0` 模式下正常編譯。
- **跨平台協定測試**：於 Linux 環境執行 Go 協定與加密單元測試，包含 FairPlay 1024 組參考測試資料比對，並交叉編譯完整 Windows 後端。

---

## `airplayd` 後端 CLI 參數

`airplayd` 通常由 Flutter 前端作為 sidecar 自動啟動與管理，亦可獨立執行以進行測試或離線錄製除錯：

| 參數 | 預設值 | 說明 |
| :--- | :--- | :--- |
| `-name` | `"鏡屏"` | 設定檔首次建立時使用的 AirPlay 接收器名稱；之後以儲存的名稱為準 |
| `-data` | 執行檔旁的 `config` 目錄 | 設定檔（`config.json`）與配對金鑰儲存資料夾 |
| `-port` | `0`（優先嘗試 `7000`） | AirPlay 控制埠；設為 `0` 時優先使用 `7000` 否則自動挑選 |
| `-api-port` | `0`（自動挑選） | 提供給 Flutter UI 溝通的本機 HTTP API 埠 |
| `-width` | `0` | 虛擬顯示器寬度（設為 `0` 時讀取 `config.json`） |
| `-height` | `0` | 虛擬顯示器高度（設為 `0` 時讀取 `config.json`） |
| `-fps` | `0` | 虛擬顯示器最高幀率（設為 `0` 時讀取 `config.json`） |
| `-record` | 空字串 | 將接收到的原始 H.264 鏡像串流儲存至指定資料夾供離線重播除錯 |
| `-shm` | `Local\YingpingFrames` | 供 UI 讀取影格的 Windows 共享記憶體區段名稱 |
| `-log` | 空字串 | 指定日誌檔案並啟用詳細記錄；未指定時仍會寫入預設的 `logs/airplayd.log` |
| `-parent-pid` | `0` | 父行程 PID（由前端傳入，當 UI 關閉時自動結束後端） |
| `-v` | `false` | 啟用詳細日誌輸出（verbose logging） |

---

## FFmpeg 偵測順序與網路排錯

### FFmpeg 搜尋順序

後端會依序從以下位置尋找 FFmpeg 共享函式庫（`.dll`），若未找到會在首次啟動時自動下載，也可透過 `backend/cmd/ffmpeg-setup` 或 `scripts/fetch-ffmpeg.ps1` 取得：

1. 環境變數 `MIRRPANE_FFMPEG_DIR` 指定路徑
2. 執行檔所在目錄及底下的 `ffmpeg/` 資料夾與快取
3. 專案目錄下的 `third_party/ffmpeg/`
4. 系統 `ffmpeg.exe` 所在目錄與環境變數 `PATH`

需使用相容的 Windows x64 shared build；僅有靜態連結的 `ffmpeg.exe` 無法提供所需 DLL。可攜版已附帶共享函式庫，可離線啟動。支援的 DLL 版本與 ABI 維護方式，請見 [第三方函式庫說明](third_party/README.md)。

### TUN / VPN 與裝置發現排錯

若 iPhone / iPad 找不到接收器，除了網路與防火牆設定，也可能與 TUN 模式或 VPN 虛擬網卡的 mDNS 廣播路由有關：

- MirrPane 會優先選擇具備 IPv4 Gateway 的 Ethernet / Wi-Fi 區網介面（包含 Hyper-V 外部交換器），並主動避開 TUN 介面、`198.18.0.0/15` 保留網段與 CGNAT 網段。
- 若仍無法發現裝置，請確認電腦與手機位於同一區網、Windows 防火牆允許 `airplayd` 與區域網路 mDNS（UDP 5353）通訊；亦可暫時停用 VPN 的 TUN 模式協助排錯。

---

## 技術依賴與致謝

### 主要技術依賴

- **後端**：`github.com/ebitengine/purego`、`github.com/grandcat/zeroconf`、`howett.net/plist`、`golang.org/x/sys`、`github.com/google/uuid`
- **前端**：Flutter Windows Desktop、自研 C++ 插件 `yingping_frames`

### 致謝

本專案參考並受益於以下開源專案：

- [UxPlay](https://github.com/FDH2/UxPlay)
- [FFmpeg](https://ffmpeg.org/)
- [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)
