#include "frame_reader.h"

#include <dxgi.h>
#include <flutter_windows.h>
#include <windows.h>

#include <cstddef>
#include <cstring>
#include <cwchar>
#include <utility>

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

constexpr uint32_t kFormatRGBA = 1;
constexpr uint32_t kFormatDXGIBGRA = 3;

constexpr LONG kConsumerFlagAttached = 1 << 0;
constexpr LONG kConsumerFlagNoGpuShare = 1 << 1;
constexpr LONG kConsumerFlagGpuShare = 1 << 2;

static_assert(offsetof(FrameReader::Slot, claim) == 0x08, "slot.claim");
static_assert(offsetof(FrameReader::Slot, frame_seq) == 0x10, "slot.frame_seq");
static_assert(offsetof(FrameReader::Slot, width) == 0x18, "slot.width");
static_assert(offsetof(FrameReader::Slot, stride) == 0x20, "slot.stride");
static_assert(offsetof(FrameReader::Slot, format) == 0x24, "slot.format");
static_assert(offsetof(FrameReader::Slot, shared_handle) == 0x38,
              "slot.shared_handle");
static_assert(sizeof(FrameReader::Slot) == 64, "slot size");

static_assert(offsetof(FrameReader::Control, version) == 0x008, "ctrl.version");
static_assert(offsetof(FrameReader::Control, slot_stride) == 0x014,
              "ctrl.slot_stride");
static_assert(offsetof(FrameReader::Control, slot_data_offset) == 0x018,
              "ctrl.data_offset");
static_assert(offsetof(FrameReader::Control, stride) == 0x028, "ctrl.stride");
static_assert(offsetof(FrameReader::Control, generation) == 0x030,
              "ctrl.generation");
static_assert(offsetof(FrameReader::Control, publish) == 0x040, "ctrl.publish");
static_assert(offsetof(FrameReader::Control, frame_seq) == 0x048,
              "ctrl.frame_seq");
static_assert(offsetof(FrameReader::Control, dropped) == 0x068, "ctrl.dropped");
static_assert(offsetof(FrameReader::Control, adapter_luid) == 0x070,
              "ctrl.adapter_luid");
static_assert(offsetof(FrameReader::Control, consumer_flags) == 0x084,
              "ctrl.consumer_flags");
static_assert(offsetof(FrameReader::Control, consumer_qpc) == 0x088,
              "ctrl.consumer_qpc");
static_assert(offsetof(FrameReader::Control, consumer_slot) == 0x090,
              "ctrl.consumer_slot");
static_assert(offsetof(FrameReader::Control, reserved_d) == 0x098,
              "ctrl.reserved_d");
static_assert(offsetof(FrameReader::Control, slots) == 0x100, "ctrl.slots");
static_assert(sizeof(FrameReader::Control) == 0x1000, "control size");

uint64_t AtomicLoad64(volatile uint64_t* p) {
  return static_cast<uint64_t>(InterlockedCompareExchange64(
      reinterpret_cast<volatile LONG64*>(p), 0, 0));
}

uint32_t AtomicLoad32(volatile uint32_t* p) {
  return static_cast<uint32_t>(InterlockedCompareExchange(
      reinterpret_cast<volatile LONG*>(p), 0, 0));
}

void ReleaseSlot(FrameReader::Slot* slot) {
  InterlockedExchange(reinterpret_cast<volatile LONG*>(&slot->claim),
                      kClaimFree);
}

// The shared counter the producer reads for its lag metric. The Go control
// block names this word ConsumerAdopted (publisher.go, offset 0x098); the C++
// header still calls it reserved_d[0], so it is written through that slot to
// keep the header's layout contract untouched.
volatile uint64_t& AdoptedCounter(FrameReader::Control* ctrl) {
  return ctrl->reserved_d[0];
}

uint64_t QueryCounter() {
  LARGE_INTEGER counter{};
  if (!QueryPerformanceCounter(&counter)) {
    return 0;
  }
  return static_cast<uint64_t>(counter.QuadPart);
}

}  // namespace

FrameReader::FrameReader() = default;

FrameReader::~FrameReader() {
  Stop();
  Unmap();
}

void FrameReader::Start(FlutterDesktopPluginRegistrarRef registrar) {
  if (registrar == nullptr || pixel_texture_id_ >= 0) {
    return;
  }
  registrar_ = registrar;
  texture_registrar_ = FlutterDesktopRegistrarGetTextureRegistrar(registrar);
  messenger_ = FlutterDesktopPluginRegistrarGetMessenger(registrar);
  if (texture_registrar_ == nullptr) {
    return;
  }

  FlutterDesktopTextureInfo pixel_info = {};
  pixel_info.type = kFlutterDesktopPixelBufferTexture;
  pixel_info.pixel_buffer_config.callback = &FrameReader::OnPixelFrame;
  pixel_info.pixel_buffer_config.user_data = this;
  pixel_texture_id_ = FlutterDesktopTextureRegistrarRegisterExternalTexture(
      texture_registrar_, &pixel_info);

  // Live Windows testing found flicker with shared surfaces. Keep the pixel
  // buffer as the default and enable the DXGI bridge only for testing.
  wchar_t experimental_dxgi[2] = {};
  if (GetEnvironmentVariableW(L"MIRRPANE_EXPERIMENTAL_DXGI", experimental_dxgi,
                             ARRAYSIZE(experimental_dxgi)) == 1 &&
      experimental_dxgi[0] == L'1') {
    InitD3D11();
  }
  if (d3d11_device_ != nullptr) {
    FlutterDesktopTextureInfo gpu_info = {};
    gpu_info.type = kFlutterDesktopGpuSurfaceTexture;
    gpu_info.gpu_surface_config.struct_size =
        sizeof(FlutterDesktopGpuSurfaceTextureConfig);
    gpu_info.gpu_surface_config.type =
        kFlutterDesktopGpuSurfaceTypeDxgiSharedHandle;
    gpu_info.gpu_surface_config.callback = &FrameReader::OnGpuSurface;
    gpu_info.gpu_surface_config.user_data = this;
    gpu_texture_id_ = FlutterDesktopTextureRegistrarRegisterExternalTexture(
        texture_registrar_, &gpu_info);
  }

  if (messenger_ != nullptr) {
    FlutterDesktopMessengerSetCallback(messenger_, kChannelName,
                                       &FrameReader::OnChannelMessage, this);
  }

  running_.store(true);
  EnsureMapped();
  waiter_ = std::thread(&FrameReader::WakeLoop, this);
}

void FrameReader::Stop() {
  if (running_.exchange(false) && waiter_.joinable()) {
    waiter_.join();
  }
  if (messenger_ != nullptr) {
    if (FlutterDesktopMessengerIsAvailable(messenger_)) {
      FlutterDesktopMessengerSetCallback(messenger_, kChannelName, nullptr,
                                         nullptr);
    }
    messenger_ = nullptr;
  }
}

void FrameReader::InitD3D11() {
  Microsoft::WRL::ComPtr<IDXGIAdapter> adapter;
  if (FlutterDesktopPluginRegistrarGetGraphicsAdapter(
          registrar_, adapter.GetAddressOf()) && adapter != nullptr) {
    D3D11CreateDevice(adapter.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr,
                      D3D11_CREATE_DEVICE_BGRA_SUPPORT, nullptr, 0,
                      D3D11_SDK_VERSION, d3d11_device_.GetAddressOf(), nullptr,
                      validation_context_.GetAddressOf());
  }
}

bool FrameReader::ValidateGpuHandle(uint32_t index, uint64_t generation,
                                    const Slot& slot) {
  if (d3d11_device_ == nullptr || slot.shared_handle == 0 || slot.width == 0 ||
      slot.height == 0) {
    return false;
  }
  const ValidatedHandle& cached = validated_handles_[index];
  if (cached.handle == slot.shared_handle && cached.width == slot.width &&
      cached.height == slot.height && cached.generation == generation) {
    return true;
  }

  // This device uses Flutter's actual rendering adapter. Unlike the control
  // block's adapter LUID, the slot's handle stays stable while claimed.
  Microsoft::WRL::ComPtr<ID3D11Texture2D> texture;
  const HANDLE shared_handle =
      reinterpret_cast<HANDLE>(static_cast<uintptr_t>(slot.shared_handle));
  if (FAILED(d3d11_device_->OpenSharedResource(
          shared_handle, IID_PPV_ARGS(texture.GetAddressOf()))) ||
      texture == nullptr) {
    return false;
  }

  D3D11_TEXTURE2D_DESC desc{};
  texture->GetDesc(&desc);
  if (desc.Width != slot.width || desc.Height != slot.height ||
      desc.Format != DXGI_FORMAT_B8G8R8A8_UNORM || desc.MipLevels != 1 ||
      desc.ArraySize != 1 || desc.SampleDesc.Count != 1) {
    return false;
  }

  const bool replaced_texture = cached.texture != nullptr;
  validated_handles_[index] = ValidatedHandle{
      slot.shared_handle, slot.width, slot.height, generation,
      std::move(texture)};
  // This device only opens shared textures. Flush its idle context so D3D11
  // destroys released wrappers and does not keep old frame allocations alive.
  if (replaced_texture) {
    validation_context_->Flush();
  }
  return true;
}

FrameReader::Slot* FrameReader::ClaimLatest(Control* ctrl, uint64_t publish) {
  const auto index = static_cast<uint32_t>(publish & 0x3);
  if (publish == 0 || index >= kSlotCount) {
    return nullptr;
  }
  Slot* slot = &ctrl->slots[index];
  if (InterlockedCompareExchange(reinterpret_cast<volatile LONG*>(&slot->claim),
                                 kClaimReader, kClaimFree) != kClaimFree) {
    return nullptr;
  }
  if (slot->frame_seq != (publish >> 2)) {
    ReleaseSlot(slot);
    return nullptr;
  }
  return slot;
}

bool FrameReader::CanUseGpu(Control* ctrl, uint64_t publish, const Slot& slot) {
  const uint64_t generation = AtomicLoad64(&ctrl->generation);
  if (slot.writer_pid != AtomicLoad32(&ctrl->producer_pid) ||
      (AtomicLoad32(&ctrl->consumer_flags) & kConsumerFlagNoGpuShare) != 0) {
    return false;
  }
  const auto index = static_cast<uint32_t>(publish & 0x3);
  if (gpu_texture_id_ >= 0 && ValidateGpuHandle(index, generation, slot)) {
    return true;
  }
  // Ignore a failed import from a sidecar that was replaced during the probe.
  if (generation == AtomicLoad64(&ctrl->generation) &&
      slot.writer_pid == AtomicLoad32(&ctrl->producer_pid)) {
    InterlockedOr(reinterpret_cast<volatile LONG*>(&ctrl->consumer_flags),
                  kConsumerFlagNoGpuShare);
  }
  return false;
}

// Answers the Dart side. The request is a one-byte command:
//
//	'T' -> the active texture id (GPU surface when zero-copy is active, else pixel buffer)
//	'S' -> the current publish word (0 when no frame has arrived)
//	'D' -> the current frame size, width then height, each a little-endian
//	       uint32 (both 0 before the first frame)
//	'F' -> pictures the engine has actually taken
//	'W' -> toggle/set borderless fullscreen
//	'P' -> toggle/set always-on-top
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
    const int64_t active_id = self->texture_id();
    value = active_id;
    const int64_t prev_id = self->last_reported_texture_id_.exchange(active_id);
    if (active_id >= 0 && active_id != prev_id &&
        self->texture_registrar_ != nullptr && self->EnsureMapped()) {
      auto* ctrl = reinterpret_cast<Control*>(self->base_);
      if (AtomicLoad64(&ctrl->publish) != 0) {
        FlutterDesktopTextureRegistrarMarkExternalTextureFrameAvailable(
            self->texture_registrar_, active_id);
      }
    }
  } else if (command == 'F') {
    value = static_cast<int64_t>(self->adopted_.load());
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
  FlutterDesktopMessengerSendResponse(messenger, message->response_handle,
                                      reply, sizeof(reply));
}

bool FrameReader::EnsureMapped() {
  std::lock_guard<std::mutex> lock(map_mutex_);
  if (!running_.load()) {
    return false;
  }
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
      ctrl->slot_data_offset != kSlotDataOffset || ctrl->slot_stride == 0) {
    UnmapViewOfFile(view);
    CloseHandle(mapping);
    return false;
  }

  base_ = static_cast<uint8_t*>(view);
  mapping_ = mapping;

  const bool can_use_gpu = d3d11_device_ != nullptr && gpu_texture_id_ >= 0;
  const LONG initial_flags = kConsumerFlagAttached |
                             (can_use_gpu ? kConsumerFlagGpuShare
                                          : kConsumerFlagNoGpuShare);

  InterlockedExchange(reinterpret_cast<volatile LONG*>(&ctrl->consumer_pid),
                      static_cast<LONG>(GetCurrentProcessId()));
  InterlockedExchange(reinterpret_cast<volatile LONG*>(&ctrl->consumer_flags),
                      initial_flags);

  wchar_t event_name[128];
  wcsncpy_s(event_name, kMappingName, _TRUNCATE);
  wcscat_s(event_name, kEventNameSuffix);
  event_ = OpenEventW(SYNCHRONIZE, FALSE, event_name);
  return true;
}

void FrameReader::Unmap() {
  std::lock_guard<std::mutex> map_lock(map_mutex_);
  std::lock_guard<std::mutex> slot_lock(slot_mutex_);
  if (held_ != nullptr) {
    ReleaseSlot(held_);
    held_ = nullptr;
    held_format_ = 0;
  }
  if (previous_ != nullptr) {
    ReleaseSlot(previous_);
    previous_ = nullptr;
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

bool FrameReader::AdoptLatest(uint32_t want_format) {
  auto* ctrl = reinterpret_cast<Control*>(base_);
  if (ctrl == nullptr) {
    return false;
  }

  const uint64_t publish = AtomicLoad64(&ctrl->publish);
  if (publish != 0 && publish != last_publish_) {
    Slot* slot = ClaimLatest(ctrl, publish);
    if (slot != nullptr) {
      if (slot->format == want_format &&
          (want_format != kFormatDXGIBGRA || CanUseGpu(ctrl, publish, *slot))) {
        SetHeldFrame(ctrl, publish, slot);
      } else {
        if (slot->format == kFormatDXGIBGRA && want_format == kFormatDXGIBGRA) {
          active_gpu_.store(false);
        }
        ReleaseSlot(slot);
      }
    }
  }
  return held_ != nullptr && held_format_ == want_format;
}

void FrameReader::SetHeldFrame(Control* ctrl, uint64_t publish, Slot* slot) {
  if (held_ != nullptr) {
    if (held_format_ == kFormatDXGIBGRA) {
      previous_ = held_;
    } else {
      ReleaseSlot(held_);
    }
  }
  const auto index = static_cast<uint32_t>(publish & 0x3);
  held_ = slot;
  held_index_ = index;
  held_format_ = slot->format;
  last_publish_ = publish;
  adopted_.fetch_add(1);

  InterlockedExchange(reinterpret_cast<volatile LONG*>(&ctrl->consumer_slot),
                      static_cast<LONG>(index));
  InterlockedExchange64(reinterpret_cast<volatile LONG64*>(&ctrl->consumer_qpc),
                        static_cast<LONG64>(QueryCounter()));
  InterlockedIncrement64(
      reinterpret_cast<volatile LONG64*>(&AdoptedCounter(ctrl)));

  if (held_format_ == kFormatRGBA) {
    FlutterDesktopPixelBuffer& buffer = buffers_[index];
    buffer.buffer = base_ + kSlotDataOffset +
                    static_cast<uint64_t>(index) * ctrl->slot_stride;
    buffer.width = slot->width;
    buffer.height = slot->height;
    buffer.release_callback = &FrameReader::OnRelease;
    buffer.release_context = this;
  } else {
    FlutterDesktopGpuSurfaceDescriptor& desc = gpu_descriptors_[index];
    desc.struct_size = sizeof(FlutterDesktopGpuSurfaceDescriptor);
    desc.handle =
        reinterpret_cast<void*>(static_cast<uintptr_t>(slot->shared_handle));
    desc.width = slot->width;
    desc.height = slot->height;
    desc.visible_width = slot->width;
    desc.visible_height = slot->height;
    desc.format = kFlutterDesktopPixelFormatBGRA8888;
    desc.release_callback = &FrameReader::OnRelease;
    desc.release_context = this;
  }
}

const FlutterDesktopPixelBuffer* FrameReader::OnPixelFrame(size_t /*width*/,
                                                           size_t /*height*/,
                                                           void* user_data) {
  auto* self = static_cast<FrameReader*>(user_data);
  if (!self->EnsureMapped()) {
    return nullptr;
  }
  std::lock_guard<std::mutex> lock(self->slot_mutex_);
  if (!self->AdoptLatest(kFormatRGBA)) {
    return nullptr;
  }
  return &self->buffers_[self->held_index_];
}

const FlutterDesktopGpuSurfaceDescriptor* FrameReader::OnGpuSurface(
    size_t /*width*/, size_t /*height*/, void* user_data) {
  auto* self = static_cast<FrameReader*>(user_data);
  if (!self->EnsureMapped()) {
    return nullptr;
  }
  std::lock_guard<std::mutex> lock(self->slot_mutex_);
  if (!self->AdoptLatest(kFormatDXGIBGRA)) {
    return nullptr;
  }
  return &self->gpu_descriptors_[self->held_index_];
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

void FrameReader::OnRelease(void* release_context) {
  auto* self = static_cast<FrameReader*>(release_context);
  std::lock_guard<std::mutex> lock(self->slot_mutex_);
  if (self->previous_ != nullptr) {
    ReleaseSlot(self->previous_);
    self->previous_ = nullptr;
  }
}

void FrameReader::WakeLoop() {
  uint64_t last_seen = 0;
  uint64_t last_generation = 0;
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

    const uint64_t generation = AtomicLoad64(&ctrl->generation);
    if (generation != 0 && generation != last_generation) {
      last_generation = generation;
      active_gpu_.store(false);
      if (d3d11_device_ != nullptr && gpu_texture_id_ >= 0) {
        InterlockedAnd(reinterpret_cast<volatile LONG*>(&ctrl->consumer_flags),
                       ~kConsumerFlagNoGpuShare);
      }
    }

    const uint64_t publish = AtomicLoad64(&ctrl->publish);
    if (publish == 0 || publish == last_seen) {
      continue;
    }
    int64_t texture = -1;
    {
      std::lock_guard<std::mutex> lock(slot_mutex_);
      const bool already_held = publish == last_publish_;
      Slot* slot = already_held ? held_ : ClaimLatest(ctrl, publish);
      if (slot == nullptr) {
        continue;
      }
      last_seen = publish;
      if (slot->format == kFormatDXGIBGRA) {
        // Probe before switching the widget to GPU. Subsequent handles are
        // opened only if the raster thread actually adopts the picture.
        if (active_gpu_.load() || CanUseGpu(ctrl, publish, *slot)) {
          active_gpu_.store(true);
          texture = gpu_texture_id_;
        } else {
          active_gpu_.store(false);
        }
      } else if (slot->format == kFormatRGBA) {
        active_gpu_.store(false);
        texture = pixel_texture_id_;
      }
      if (!already_held) {
        ReleaseSlot(slot);
      }
    }
    if (texture >= 0) {
      FlutterDesktopTextureRegistrarMarkExternalTextureFrameAvailable(
          texture_registrar_, texture);
    }
  }
}
