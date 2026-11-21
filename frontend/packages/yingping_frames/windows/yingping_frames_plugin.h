#ifndef FLUTTER_PLUGIN_YINGPING_FRAMES_PLUGIN_H_
#define FLUTTER_PLUGIN_YINGPING_FRAMES_PLUGIN_H_

#include <flutter/method_channel.h>
#include <flutter/plugin_registrar_windows.h>

#include <memory>

namespace yingping_frames {

class YingpingFramesPlugin : public flutter::Plugin {
 public:
  static void RegisterWithRegistrar(flutter::PluginRegistrarWindows *registrar);

  YingpingFramesPlugin();

  virtual ~YingpingFramesPlugin();

  // Disallow copy and assign.
  YingpingFramesPlugin(const YingpingFramesPlugin&) = delete;
  YingpingFramesPlugin& operator=(const YingpingFramesPlugin&) = delete;

  // Called when a method is called on this plugin's channel from Dart.
  void HandleMethodCall(
      const flutter::MethodCall<flutter::EncodableValue> &method_call,
      std::unique_ptr<flutter::MethodResult<flutter::EncodableValue>> result);
};

}  // namespace yingping_frames

#endif  // FLUTTER_PLUGIN_YINGPING_FRAMES_PLUGIN_H_
