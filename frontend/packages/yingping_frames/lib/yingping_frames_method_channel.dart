import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

import 'yingping_frames_platform_interface.dart';

/// An implementation of [YingpingFramesPlatform] that uses method channels.
class MethodChannelYingpingFrames extends YingpingFramesPlatform {
  /// The method channel used to interact with the native platform.
  @visibleForTesting
  final methodChannel = const MethodChannel('yingping_frames');

  @override
  Future<String?> getPlatformVersion() async {
    final version = await methodChannel.invokeMethod<String>(
      'getPlatformVersion',
    );
    return version;
  }
}
