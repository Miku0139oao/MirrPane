#ifndef RUNNER_FRAME_READER_H_
#define RUNNER_FRAME_READER_H_

#include <windows.h>

#include <flutter_messenger.h>
#include <flutter_plugin_registrar.h>
#include <flutter_texture_registrar.h>

#include <atomic>
#include <cstdint>
#include <mutex>
#include <thread>

// FrameReader attaches to the shared-memory section the Go sidecar publishes
// decoded pictures into and exposes it to Flutter as a pixel-buffer texture.
//
// The section layout is a contract with backend/internal/frames/publisher.go.
// Every offset used here is the same number on both sides, and both sides assert
// it, so a layout drift breaks a build instead of producing a scrambled image.
class FrameReader {
 public:
  FrameReader();
  ~FrameReader();

  // Registers the texture and starts the wake-up thread. Call once, after the
  // engine is running.
  void Start(FlutterDesktopPluginRegistrarRef registrar);

  // Stops the wake-up thread and releases the mapping.
  void Stop();

  int64_t texture_id() const { return texture_id_; }

  // --- layout contract (mirrors the Go struct) -------------------------------
  // Public because the .cpp asserts their offsets from namespace scope.
  struct Slot {
    uint64_t seq;
    uint32_t claim;
    uint32_t flags;
    uint64_t frame_seq;
    uint32_t width;
    uint32_t height;
    uint32_t stride;
    uint32_t format;
    uint64_t write_qpc;
    uint32_t writer_pid;
    uint32_t reserved0;
    uint64_t reserved1;
  };

  struct Control {
    uint64_t magic;
    uint32_t version;
    uint32_t header_bytes;
    uint32_t slot_count;
    uint32_t slot_stride;
    uint64_t slot_data_offset;
    uint32_t width;
    uint32_t height;
    uint32_t stride;
    uint32_t pixel_format;
    uint64_t generation;
    uint32_t producer_pid;
    uint32_t flags;
    uint64_t publish;
    uint64_t frame_seq;
    uint64_t last_publish_qpc;
    uint64_t heartbeat_qpc;
    uint64_t frame_pts;
    uint64_t dropped;
    uint64_t reserved_a;
    uint64_t reserved_b;
    uint32_t consumer_pid;
    uint32_t consumer_flags;
    uint64_t consumer_qpc;
    uint32_t consumer_slot;
    uint32_t reserved_c;
    uint64_t reserved_d[13];
    Slot slots[3];
    uint8_t tail[0x1000 - 0x1C0];
  };

 private:
  static const FlutterDesktopPixelBuffer* OnFrame(size_t width, size_t height,
                                                  void* user_data);
  static void OnRelease(void* release_context);
  static void OnChannelMessage(FlutterDesktopMessengerRef messenger,
                               const FlutterDesktopMessage* message,
                               void* user_data);

  bool EnsureMapped();
  void Unmap();
  void WakeLoop();
  HWND RootWindow() const;
  bool SetFullscreen(bool enable);
  bool SetAlwaysOnTop(bool enable);

  FlutterDesktopPluginRegistrarRef registrar_ = nullptr;
  FlutterDesktopTextureRegistrarRef texture_registrar_ = nullptr;
  FlutterDesktopMessengerRef messenger_ = nullptr;
  int64_t texture_id_ = -1;
  bool fullscreen_ = false;
  bool topmost_ = false;
  LONG_PTR saved_style_ = 0;
  WINDOWPLACEMENT saved_placement_{sizeof(WINDOWPLACEMENT)};

  HANDLE mapping_ = nullptr;
  HANDLE event_ = nullptr;
  uint8_t* base_ = nullptr;
  std::mutex map_mutex_;

  uint64_t last_publish_ = 0;
  // adopted_ counts the pictures the engine has actually taken, which is the
  // only honest measure of what reaches the screen.
  uint64_t adopted_ = 0;
  // held_ is the slot currently claimed for the engine to render from. It is
  // kept between callbacks so a redraw with no new picture shows the last one
  // instead of blanking, and released when a newer picture is adopted.
  Slot* held_ = nullptr;
  uint32_t held_index_ = 0;
  FlutterDesktopPixelBuffer buffers_[3];

  std::thread waiter_;
  std::atomic<bool> running_{false};
};

#endif  // RUNNER_FRAME_READER_H_
