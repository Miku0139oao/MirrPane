; MirrPane per-user installer.
;
; Built by build-installer.ps1, which passes:
;   /DAppVersion=<version> /DSourceDir=<packaged folder>
; The installer needs no administrator rights: it installs under the user's
; LocalAppData, where the receiver may write its config, logs and screenshots
; beside the executable as the portable build does.

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "..\dist\鏡屏-0.0.0"
#endif
#define AppName "鏡屏 MirrPane"
#define AppExe "MirrPane.exe"

[Setup]
AppId={{9F1B0C86-3C7A-4D2E-9A61-5B0F5D2C7E31}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher=MirrPane
AppPublisherURL=https://github.com/Miku0139oao/MirrPane
AppSupportURL=https://github.com/Miku0139oao/MirrPane/issues
AppUpdatesURL=https://github.com/Miku0139oao/MirrPane/releases
DefaultDirName={localappdata}\Programs\MirrPane
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
OutputBaseFilename=MirrPane-{#AppVersion}-setup
SetupIconFile=..\assets\mirrpane.ico
UninstallDisplayIcon={app}\{#AppExe}
LicenseFile=..\LICENSE
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
CloseApplications=yes
RestartApplications=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "{#SourceDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\{#AppName}"; Filename: "{app}\{#AppExe}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExe}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#AppExe}"; Description: "{cm:LaunchProgram,{#AppName}}"; Flags: nowait postinstall skipifsilent

[UninstallDelete]
Type: filesandordirs; Name: "{app}\config"
Type: filesandordirs; Name: "{app}\logs"
Type: filesandordirs; Name: "{app}\screenshots"
Type: filesandordirs; Name: "{app}\ffmpeg"
; Pre-merge layout: the receiver and its runtime lived under backend\.
Type: filesandordirs; Name: "{app}\backend\ffmpeg"
Type: files; Name: "{app}\backend\airplayd.exe"
Type: files; Name: "{app}\backend\*.dll"
