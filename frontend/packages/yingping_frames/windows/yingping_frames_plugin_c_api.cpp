#include "include/yingping_frames/yingping_frames_plugin_c_api.h"

#include <flutter/plugin_registrar_windows.h>

#include <memory>

#include "frame_reader.h"
#include "yingping_frames_plugin.h"

namespace {

// The reader lives for the whole process: the engine holds its texture callback
// and a wake-up thread posts into it, so tearing it down at an arbitrary point
// would race the raster thread.
std::unique_ptr<FrameReader>& Reader() {
  static std::unique_ptr<FrameReader> reader;
  return reader;
}

}  // namespace

void YingpingFramesPluginCApiRegisterWithRegistrar(
    FlutterDesktopPluginRegistrarRef registrar) {
  yingping_frames::YingpingFramesPlugin::RegisterWithRegistrar(
      flutter::PluginRegistrarManager::GetInstance()
          ->GetRegistrar<flutter::PluginRegistrarWindows>(registrar));

  // The raw registrar is what the texture and messenger APIs take, so the
  // reader is started here rather than from the plugin's Dart-facing half.
  if (!Reader()) {
    Reader() = std::make_unique<FrameReader>();
    Reader()->Start(registrar);
  }
}
