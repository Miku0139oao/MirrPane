import 'package:flutter_test/flutter_test.dart';
import 'package:yingping_frames/yingping_frames.dart';
import 'package:yingping_frames/yingping_frames_platform_interface.dart';
import 'package:yingping_frames/yingping_frames_method_channel.dart';
import 'package:plugin_platform_interface/plugin_platform_interface.dart';

class MockYingpingFramesPlatform
    with MockPlatformInterfaceMixin
    implements YingpingFramesPlatform {
  @override
  Future<String?> getPlatformVersion() => Future.value('42');
}

void main() {
  final YingpingFramesPlatform initialPlatform = YingpingFramesPlatform.instance;

  test('$MethodChannelYingpingFrames is the default instance', () {
    expect(initialPlatform, isInstanceOf<MethodChannelYingpingFrames>());
  });

  test('getPlatformVersion', () async {
    YingpingFrames yingpingFramesPlugin = YingpingFrames();
    MockYingpingFramesPlatform fakePlatform = MockYingpingFramesPlatform();
    YingpingFramesPlatform.instance = fakePlatform;

    expect(await yingpingFramesPlugin.getPlatformVersion(), '42');
  });
}
