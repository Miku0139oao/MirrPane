#include "frame_reader.h"

#include <flutter_windows.h>
#include <windows.h>

#include <cstddef>
#include <cstring>
#include <cwchar>

namespace {

// Names shared with backend/internal/frames. Both sides must agree.
constexpr wchar_t kMappingName[] = L"Local\\YingpingFrames";
constexpr wchar_t kEventNameSuffix[] = L".frame";
constexpr char kChannelName[] = "dev.yingping/frames";

constexpr uint64_t kMagic = 0x314D4853524D4641ull;  // "AFRMSHM1"
constexpr uint32_t kVersion = 1;
constexpr uint32_t kSlotCount = 3;
constexpr uint32_t kSlotDataOffset = 0x10000;
constexpr uint32_t kClaimFree = 0;
constexpr uint32_t kClaimReader = 2;

static_assert(offsetof(FrameReader::Slot, claim) == 0x08, "slot.claim");
static_assert(offsetof(FrameReader::Slot, frame_seq) == 0x10, "slot.frame_seq");
static_assert(offsetof(FrameReader::Slot, width) == 0x18, "slot.width");
static_assert(offsetof(FrameReader::Slot, stride) == 0x20, "slot.stride");
static_assert(offsetof(FrameReader::Slot, format) == 0x24, "slot.format");
static_assert(sizeof(FrameReader::Slot) == 64, "slot size");

static_assert(offsetof(FrameReader::Control, version) == 0x008, "ctrl.version");
static_assert(offsetof(FrameReader::Control, slot_stride) == 0x014, "ctrl.slot_stride");
static_assert(offsetof(FrameReader::Control, slot_data_offset) == 0x018, "ctrl.data_offset");
static_assert(offsetof(FrameReader::Control, stride) == 0x028, "ctrl.stride");
static_assert(offsetof(FrameReader::Control, publish) == 0x040, "ctrl.publish");
static_assert(offsetof(FrameReader::Control, frame_seq) == 0x048, "ctrl.frame_seq");
static_assert(offsetof(FrameReader::Control, dropped) == 0x068, "ctrl.dropped");
static_assert(offsetof(FrameReader::Control, slots) == 0x100, "ctrl.slots");
static_assert(sizeof(FrameReader::Control) == 0x1000, "control size");

// A frame older than this many publish words behind is not worth showing; the
// consumer simply renders the newest one.
uint64_t AtomicLoad64(volatile uint64_t* p) {
  return static_cast<uint64_t>(
      InterlockedCompareExchange64(reinterpret_cast<volatile LONG64*>(p), 0, 0));
}

}  // namespace

FrameReader::FrameReader() = default;

FrameReader::~FrameReader() { Stop(); }

void FrameReader::Start(FlutterDesktopPluginRegistrarRef registrar) {
  if (registrar == nullptr || texture_id_ >= 0) {
    return;
  }
  registrar_ = registrar;
  texture_registrar_ = FlutterDesktopRegistrarGetTextureRegistrar(registrar);
  messenger_ = FlutterDesktopPluginRegistrarGetMessenger(registrar);
  if (texture_registrar_ == nullptr) {
    return;
  }

  FlutterDesktopTextureInfo info = {};
  info.type = kFlutterDesktopPixelBufferTexture;
  info.pixel_buffer_config.callback = &FrameReader::OnFrame;
  info.pixel_buffer_config.user_data = this;
  texture_id_ =
      FlutterDesktopTextureRegistrarRegisterExternalTexture(texture_registrar_,
                                                            &info);

  if (messenger_ != nullptr) {
    FlutterDesktopMessengerSetCallback(messenger_, kChannelName,
                                       &FrameReader::OnChannelMessage, this);
  }

  EnsureMapped();
  running_.store(true);
  waiter_ = std::thread(&FrameReader::WakeLoop, this);
}

void FrameReader::Stop() {
  if (running_.exchange(false) && waiter_.joinable()) {
    waiter_.join();
  }
  if (messenger_ != nullptr) {
    FlutterDesktopMessengerSetCallback(messenger_, kChannelName, nullptr,
                                       nullptr);
    messenger_ = nullptr;
  }
  Unmap();
}

// Answers the Dart side. The request is a one-byte command:
//
//	'T' -> the texture id the engine assigned
//	'S' -> the current publish word (0 when no frame has arrived)
//	'D' -> the current frame size, width then height, each a little-endian
//	       uint32 (both 0 before the first frame)
//
// The id is chosen by the engine at runtime, so Dart cannot know it any other
// way; the publish word lets Dart tell "waiting" from "mirroring" without
// guessing; the size lets the picture keep its aspect ratio instead of being
// stretched to the window.
void FrameReader::OnChannelMessage(FlutterDesktopMessengerRef messenger,
                                   const FlutterDesktopMessage* message,
                                   void* user_data) {
  if (message == nullptr || message->response_handle == nullptr) {
    return;
  }
  auto* self = static_cast<FrameReader*>(user_data);
  const char command =
      message->message_size > 0 ? static_cast<char>(message->message[0]) : 'T';

  int64_t value = 0;
  if (command == 'D') {
    uint32_t size[2] = {0, 0};
    if (self->EnsureMapped()) {
      auto* ctrl = reinterpret_cast<Control*>(self->base_);
      const uint64_t publish = AtomicLoad64(&ctrl->publish);
      const uint32_t index = static_cast<uint32_t>(publish & 0x3);
      if (publish != 0 && index < kSlotCount) {
        size[0] = ctrl->slots[index].width;
        size[1] = ctrl->slots[index].height;
      }
    }
    uint8_t reply[8];
    std::memcpy(reply, size, sizeof(reply));
    FlutterDesktopMessengerSendResponse(messenger, message->response_handle,
                                        reply, sizeof(reply));
    return;
  }

  if (command == 'T') {
    value = self->texture_id_;
  } else if (command == 'F') {
    // Pictures the engine has taken: the real displayed frame count.
    value = static_cast<int64_t>(self->adopted_);
  } else if (command == 'S') {
    if (self->EnsureMapped()) {
      auto* ctrl = reinterpret_cast<Control*>(self->base_);
      value = static_cast<int64_t>(AtomicLoad64(&ctrl->publish));
    }
  } else if (command == 'W') {
    const bool want = message->message_size >= 2
                          ? (message->message[1] != 0)
                          : !self->fullscreen_;
    value = self->SetFullscreen(want) ? 1 : 0;
  } else if (command == 'P') {
    const bool want = message->message_size >= 2
                          ? (message->message[1] != 0)
                          : !self->topmost_;
    value = self->SetAlwaysOnTop(want) ? 1 : 0;
  }

  uint8_t reply[8];
  std::memcpy(reply, &value, sizeof(reply));
  FlutterDesktopMessengerSendResponse(messenger, message->response_handle, reply,
                                      sizeof(reply));
}

bool FrameReader::EnsureMapped() {
  std::lock_guard<std::mutex> lock(map_mutex_);
  if (base_ != nullptr) {
    return true;
  }

  HANDLE mapping =
      OpenFileMappingW(FILE_MAP_READ | FILE_MAP_WRITE, FALSE, kMappingName);
  if (mapping == nullptr) {
    return false;
  }
  void* view = MapViewOfFile(mapping, FILE_MAP_READ | FILE_MAP_WRITE, 0, 0, 0);
  if (view == nullptr) {
    CloseHandle(mapping);
    return false;
  }

  auto* ctrl = static_cast<Control*>(view);
  if (ctrl->magic != kMagic || ctrl->version != kVersion ||
      ctrl->slot_count != kSlotCount ||
      ctrl->slot_data_offset != kSlotDataOffset ||
      ctrl->slot_stride == 0) {
    // Somebody else owns the name, or the producer is a different build.
    UnmapViewOfFile(view);
    CloseHandle(mapping);
    return false;
  }

  base_ = static_cast<uint8_t*>(view);
  mapping_ = mapping;

  // The event is only a wake-up hint; polling still works without it.
  wchar_t event_name[128];
  wcsncpy_s(event_name, kMappingName, _TRUNCATE);
  wcscat_s(event_name, kEventNameSuffix);
  event_ = OpenEventW(SYNCHRONIZE, FALSE, event_name);
  return true;
}

void FrameReader::Unmap() {
  std::lock_guard<std::mutex> lock(map_mutex_);
  // Give the claim back before the view disappears.
  if (held_ != nullptr) {
    InterlockedExchange(reinterpret_cast<volatile LONG*>(&held_->claim),
                        kClaimFree);
    held_ = nullptr;
  }
  if (event_ != nullptr) {
    CloseHandle(event_);
    event_ = nullptr;
  }
  if (base_ != nullptr) {
    UnmapViewOfFile(base_);
    base_ = nullptr;
  }
  if (mapping_ != nullptr) {
    CloseHandle(mapping_);
    mapping_ = nullptr;
  }
  last_publish_ = 0;
}

// Called on the raster thread. Returns a pointer straight into the published
// slot; the engine copies the pixels while rasterising, so the buffer only has
// to stay valid for the duration of this call.
//
// When nothing new has arrived, the previous picture is returned again rather
// than nullptr. The engine asks once per rendered frame, which is often more
// often than the sender produces one; answering nullptr blanks the texture for
// that frame, which reads as a flicker.
const FlutterDesktopPixelBuffer* FrameReader::OnFrame(size_t /*width*/,
                                                      size_t /*height*/,
                                                      void* user_data) {
  auto* self = static_cast<FrameReader*>(user_data);
  if (!self->EnsureMapped()) {
    return nullptr;
  }

  auto* ctrl = reinterpret_cast<Control*>(self->base_);
  const uint64_t publish = AtomicLoad64(&ctrl->publish);
  if (publish != 0 && publish != self->last_publish_) {
    const uint32_t index = static_cast<uint32_t>(publish & 0x3);
    if (index < kSlotCount) {
      Slot* slot = &ctrl->slots[index];
      if (InterlockedCompareExchange(
              reinterpret_cast<volatile LONG*>(&slot->claim), kClaimReader,
              kClaimFree) == kClaimFree) {
        // Hand the previous slot back before adopting this one, so the reader
        // never holds two and the writer always has a free slot to fill.
        if (self->held_ != nullptr) {
          InterlockedExchange(
              reinterpret_cast<volatile LONG*>(&self->held_->claim),
              kClaimFree);
        }
        self->held_ = slot;
        self->held_index_ = index;
        self->last_publish_ = publish;
        ++self->adopted_;

        FlutterDesktopPixelBuffer& buffer = self->buffers_[index];
        buffer.buffer = self->base_ + kSlotDataOffset +
                        static_cast<uint64_t>(index) * ctrl->slot_stride;
        buffer.width = slot->width;
        buffer.height = slot->height;
        buffer.release_callback = &FrameReader::OnRelease;
        buffer.release_context = nullptr;
      }
    }
  }

  if (self->held_ == nullptr) {
    return nullptr;  // no picture has been published yet
  }
  return &self->buffers_[self->held_index_];
}

HWND FrameReader::RootWindow() const {
  if (registrar_ == nullptr) return nullptr;
  FlutterDesktopViewRef view = FlutterDesktopPluginRegistrarGetView(registrar_);
  if (view == nullptr) return nullptr;
  HWND view_hwnd = FlutterDesktopViewGetHWND(view);
  if (view_hwnd == nullptr) return nullptr;
  return GetAncestor(view_hwnd, GA_ROOT);
}

bool FrameReader::SetFullscreen(bool enable) {
  HWND hwnd = RootWindow();
  if (hwnd == nullptr) return fullscreen_;
  if (enable == fullscreen_) return fullscreen_;

  if (enable) {
    saved_style_ = GetWindowLongPtr(hwnd, GWL_STYLE);
    saved_placement_.length = sizeof(WINDOWPLACEMENT);
    GetWindowPlacement(hwnd, &saved_placement_);

    MONITORINFO mi{sizeof(MONITORINFO)};
    if (GetMonitorInfo(MonitorFromWindow(hwnd, MONITOR_DEFAULTTONEAREST), &mi)) {
      SetWindowLongPtr(hwnd, GWL_STYLE, saved_style_ & ~WS_OVERLAPPEDWINDOW);
      SetWindowPos(hwnd, HWND_TOP, mi.rcMonitor.left, mi.rcMonitor.top,
                   mi.rcMonitor.right - mi.rcMonitor.left,
                   mi.rcMonitor.bottom - mi.rcMonitor.top,
                   SWP_NOOWNERZORDER | SWP_FRAMECHANGED);
      fullscreen_ = true;
    }
  } else {
    SetWindowLongPtr(hwnd, GWL_STYLE, saved_style_);
    SetWindowPlacement(hwnd, &saved_placement_);
    SetWindowPos(hwnd, topmost_ ? HWND_TOPMOST : HWND_NOTOPMOST, 0, 0, 0, 0,
                 SWP_NOMOVE | SWP_NOSIZE | SWP_NOOWNERZORDER | SWP_FRAMECHANGED);
    fullscreen_ = false;
  }
  return fullscreen_;
}

bool FrameReader::SetAlwaysOnTop(bool enable) {
  HWND hwnd = RootWindow();
  if (hwnd == nullptr) return topmost_;
  topmost_ = enable;
  SetWindowPos(hwnd, topmost_ ? HWND_TOPMOST : HWND_NOTOPMOST, 0, 0, 0, 0,
               SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE);
  return topmost_;
}

// The engine reports the copy is done, but the claim outlives the call: keeping
// the slot lets the next redraw show the same picture. It is released when a
// newer one is adopted, or when the mapping goes away.
void FrameReader::OnRelease(void* /*release_context*/) {}

// Waits for the producer's wake-up event and tells Flutter a new frame is ready.
// The wait has a timeout so a producer that died mid-stream cannot wedge the UI.
void FrameReader::WakeLoop() {
  uint64_t last_seen = 0;
  while (running_.load()) {
    if (!EnsureMapped()) {
      Sleep(200);
      continue;
    }
    if (event_ != nullptr) {
      WaitForSingleObject(event_, 200);
    } else {
      Sleep(8);
    }
    auto* ctrl = reinterpret_cast<Control*>(base_);
    if (ctrl == nullptr) {
      continue;
    }
    uint64_t publish = AtomicLoad64(&ctrl->publish);
    if (publish != 0 && publish != last_seen) {
      last_seen = publish;
      if (texture_registrar_ != nullptr && texture_id_ >= 0) {
        FlutterDesktopTextureRegistrarMarkExternalTextureFrameAvailable(
            texture_registrar_, texture_id_);
      }
    }
  }
}
