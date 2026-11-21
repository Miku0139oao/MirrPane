
import 'yingping_frames_platform_interface.dart';

class YingpingFrames {
  Future<String?> getPlatformVersion() {
    return YingpingFramesPlatform.instance.getPlatformVersion();
  }
}
