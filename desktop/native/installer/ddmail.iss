; ddmail — Windows installer (Inno Setup 6).
; Build:  ISCC.exe installer\ddmail.iss
; Input:  target\release\ddmail-native.exe (cargo build --release)
; Output: installer\out\ddmail-setup-<version>.exe
;
; Per-user install (no UAC): %LOCALAPPDATA%\Programs\ddmail. Внешних
; зависимостей нет: тела писем рендерит собственный движок (desktop/emlrender),
; браузерного рантайма в поставке не требуется.

#define AppName "ddmail"
#define AppVersion "0.1.6"
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
