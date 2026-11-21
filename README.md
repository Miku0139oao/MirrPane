# 鏡屏 (MirrPane)

[![CI](https://github.com/Miku0139oao/MirrPane/actions/workflows/ci.yml/badge.svg)](https://github.com/Miku0139oao/MirrPane/actions/workflows/ci.yml)
[![Release](https://github.com/Miku0139oao/MirrPane/actions/workflows/release.yml/badge.svg)](https://github.com/Miku0139oao/MirrPane/actions/workflows/release.yml)

**鏡屏 (MirrPane)** 是一款專為 Windows 打造的 AirPlay 螢幕鏡像接收器，將電腦變成 iPhone / iPad 的無線外接顯示器。整包解壓即用，不寫入系統登錄檔、不需額外安裝執行期環境。

---

## 主要特色

- **D3D11VA 硬體加速解碼**：透過 FFmpeg `D3D11VA` 進行低延遲 H.264 硬體解碼，不支援的環境自動退回 CPU 軟體解碼。
- **低延遲無卡頓管線**：直通式 Annex-B access unit 解碼、零複製共享記憶體（3-slot mailbox + Windows 事件喚醒）直繪 Flutter 紋理。
- **同步音訊播放**：支援 AAC-ELD 音訊串流（`winmm waveOut` 原生驅動），具備即時音量／靜音控制與三檔抗抖動緩衝（超低延遲 30 ms / 標準平衡 40 ms / 弱網穩定 60 ms）。
- **4 分頁進階設定**：
  - **一般**：自訂 AirPlay 廣播名稱、本機連線資訊、即時統計與配對身分重設。
  - **圖形與解碼**：虛擬顯示解析度（720p / 1080p / 1440p / 4K）、幀率上限（60 / 30 fps）、解碼模式（D3D11VA / 軟體 CPU）、色彩範圍（Auto / Full / Limited）、色彩轉換演算法。
  - **畫面與色彩（GPU 即時生效）**：貼圖縮放濾鏡（`FilterQuality`）、亮度／對比度／飽和度即時調整、水平鏡像翻轉、畫面旋轉、填滿／比例切換、效能監控 HUD 四角位置與頂列隱藏時間。
  - **音訊**：音量與靜音記憶、抗抖動緩衝設定。
- **綠色可攜**：所有設定（`config/`）、截圖（`screenshots/`）與日誌（`logs/`）皆保存在程式目錄內。

---

## 快速使用

1. 前往 [Releases](https://github.com/Miku0139oao/MirrPane/releases) 下載最新 `MirrPane-*-win64.zip` 並解壓縮到任意非系統保護資料夾。
2. 執行 `MirrPane.exe`（第一次啟動時請在 Windows 防火牆提示選擇**允許私人網路存取**）。
3. 在 iPhone / iPad 開啟「控制中心」→「螢幕鏡像輸出」，選擇「**鏡屏**」。

### 快捷鍵

| 快捷鍵 | 功能 |
|---|---|
| `F11` / 雙擊畫面 | 切換全螢幕（`Esc` 離開） |
| `Ctrl + T` | 視窗置頂 |
| `R` | 畫面順時針旋轉 90° |
| `H` | 水平鏡像翻轉 |
| `Z` | 切換完整比例 / 填滿視窗 |
| `M` | 靜音切換 |
| `Ctrl + S` | 無損 PNG 截圖（存至 `screenshots\`） |
| `Tab` | 切換左下／角落效能監控 HUD |
| `Ctrl + L` | 檢視執行日誌 |
| `Ctrl + ,` | 開啟設定視窗 |

---

## 本機開發與建置

### 需求工具

- **Go** `1.26+`（需啟用 `cgo`）
- **MinGW-w64 GCC**（64-bit，例如 MSYS2 `mingw64`；Windows cgo 不使用 MSVC `cl`）
- **Flutter** `3.47+`（已啟用 Windows desktop 與 Visual Studio C++ Build Tools）

### 建置指令

```powershell
# 1. 下載並精簡 FFmpeg Windows 共用開發套件至 third_party\ffmpeg
.\scripts\fetch-ffmpeg.ps1

# 2. 執行後端測試
cd backend
$env:PATH = "$PWD\..\third_party\ffmpeg\bin;$env:PATH"
go test ./internal/...
cd ..

# 3. 完整建置 Flutter + Go sidecar 並打包為 dist\鏡屏\ 與 zip
.\package.ps1
```

---

## 授權與致謝

本專案採用 **GPLv3** 授權（詳見 [LICENSE](LICENSE)）。

AirPlay 協定實作、FairPlay 與 RAOP/Mirror 串流解析參考並移植自 [UxPlay](https://github.com/FDH2/UxPlay)（GPLv3）；視訊與音訊解碼採用 [FFmpeg](https://ffmpeg.org/)（GPLv3 共用函式庫由 [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) 提供）。
