; ddmail — Windows installer (Inno Setup 6).
; Build:  ISCC.exe installer\ddmail.iss
; Input:  target\release\ddmail-native.exe (cargo build --release)
; Output: installer\out\ddmail-setup-<version>.exe
;
; Per-user install (no UAC): %LOCALAPPDATA%\Programs\ddmail. WebView2 Runtime
; is required for message rendering — present on stock Windows 11, checked and
; bootstrapped on systems where it's missing.

#define AppName "ddmail"
#define AppVersion "0.1.0"
#define AppPublisher "letotam.ru"
#define AppURL "https://mail.letotam.ru"
#define AppExe "ddmail-native.exe"

[Setup]
AppId={{7E1B4E0D-3C64-4A5B-9C64-DDMA11000001}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}
DefaultDirName={userpf}\{#AppName}
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
OutputDir=out
OutputBaseFilename=ddmail-setup-{#AppVersion}
SetupIconFile=..\assets\ddmail.ico
UninstallDisplayIcon={app}\{#AppExe}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
; The app keeps its exe unlocked-in-theory, but a running instance must be
; closed before files are replaced on upgrade.
CloseApplications=yes

[Languages]
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"
Name: "autostart"; Description: "Запускать при входе в Windows"; GroupDescription: "Дополнительно:"

[Files]
Source: "..\target\release\{#AppExe}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{userprograms}\{#AppName}"; Filename: "{app}\{#AppExe}"
Name: "{userdesktop}\{#AppName}"; Filename: "{app}\{#AppExe}"; Tasks: desktopicon

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "{#AppName}"; ValueData: """{app}\{#AppExe}"""; \
    Flags: uninsdeletevalue; Tasks: autostart

[Run]
Filename: "{app}\{#AppExe}"; Description: "{cm:LaunchProgram,{#AppName}}"; \
    Flags: nowait postinstall skipifsilent

[UninstallDelete]
; Cache is regenerable (bodies/textures re-fetch); accounts.json holds the
; login token — remove both so uninstall leaves no credentials behind.
Type: filesandordirs; Name: "{userappdata}\ru.letotam.ddmail"

[Code]
// WebView2 Runtime detection: per-machine, then per-user (Evergreen).
function WebView2Installed(): Boolean;
var
  Ver: String;
begin
  Result :=
    RegQueryStringValue(HKLM,
      'SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}',
      'pv', Ver) or
    RegQueryStringValue(HKCU,
      'Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}',
      'pv', Ver);
  Result := Result and (Ver <> '') and (Ver <> '0.0.0.0');
end;

procedure InstallWebView2();
var
  Path: String;
  ResultCode: Integer;
begin
  Path := ExpandConstant('{tmp}\MicrosoftEdgeWebView2Setup.exe');
  if not FileExists(Path) then begin
    try
      // Evergreen bootstrapper (~2 MB), official fwlink.
      DownloadTemporaryFile(
        'https://go.microsoft.com/fwlink/p/?LinkId=2124703',
        'MicrosoftEdgeWebView2Setup.exe', '', nil);
    except
      // No network / blocked download: the app itself will show a clear
      // render error; don't fail the install over it.
      exit;
    end;
  end;
  Exec(Path, '/silent /install', '', SW_SHOW, ewWaitUntilTerminated, ResultCode);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and not WebView2Installed() then
    InstallWebView2();
end;
