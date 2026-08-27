; ddmail — Windows installer (Inno Setup 6).
; Build:  ISCC.exe installer\ddmail.iss
;         ISCC.exe /DSrcDir=%LOCALAPPDATA%\builds\cargo-target\release installer\ddmail.iss
;           — когда cargo собирает в кэш станции (CARGO_TARGET_DIR), а не in-tree.
; Input:  <SrcDir>\ddmail-native.exe (cargo build --release)
; Output: installer\out\ddmail-setup-<version>.exe
;
; Per-user install (no UAC): %LOCALAPPDATA%\Programs\ddmail. Внешних
; зависимостей нет: тела писем рендерит собственный движок (desktop/emlrender),
; браузерного рантайма в поставке не требуется.

#define AppName "ddmail"
#define AppVersion "0.1.10"
#define AppPublisher "letotam.ru"
#define AppURL "https://mail.letotam.ru"
#define AppExe "ddmail-native.exe"
; Откуда брать собранный exe. По умолчанию in-tree target, но на станциях
; cargo пишет в кэш вне дерева — тогда путь передаётся через /DSrcDir=...
#ifndef SrcDir
  #define SrcDir "..\target\release"
#endif

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
; Запущенный экземпляр надо закрыть до подмены файлов, и штатных средств Inno
; для этого не хватает — закрытие делает [Code] ниже. Здесь только страховка:
;  * `force` вместо `yes`, потому что Restart Manager просит закрыться через
;    WM_CLOSE, а ddmail на закрытие окна прячется в трей
;    (`CloseRequestResponse::HideWindow` в main.rs) — процесс живёт дальше;
;  * но и `force` не срабатывает, когда RM процесс не находит вовсе: в логе
;    установки поверх запущенного приложения стоит «RestartManager found no
;    applications using one of our files». Отсюда taskkill в InitializeSetup.
; `RestartApplications=no` — поднимать приложение обратно должен [Run], а не
; Restart Manager (он вернул бы старый процесс до подмены файлов).
CloseApplications=force
CloseApplicationsFilter=*.exe
RestartApplications=no

[Languages]
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"
Name: "autostart"; Description: "Запускать при входе в Windows"; GroupDescription: "Дополнительно:"

[Files]
Source: "{#SrcDir}\{#AppExe}"; DestDir: "{app}"; Flags: ignoreversion

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
; Тихая установка (/SILENT — обновление из скрипта) поднимает приложение сама:
; галочки «Запустить» там никто не увидит, а обновление без перезапуска
; оставляет пользователя на старом процессе.
Filename: "{app}\{#AppExe}"; Flags: nowait; Check: WizardSilent

[UninstallDelete]
; Cache is regenerable (bodies/textures re-fetch); accounts.json holds the
; login token — remove both so uninstall leaves no credentials behind.
Type: filesandordirs; Name: "{userappdata}\ru.letotam.ddmail"

[Code]
{ Запущенный ddmail мешает и установке, и удалению: exe залочен процессом.
  На Restart Manager рассчитывать нельзя — на этой связке он приложение не
  находит («RestartManager found no applications using one of our files»),
  поэтому закрываем сами и ДО всех проверок. }

function AppRunning(): Boolean;
var
  Code: Integer;
begin
  Result := False;
  { Exec не отдаёт stdout, поэтому наличие процесса читаем кодом возврата
    findstr: 0 — нашёл строку с именем процесса. }
  if Exec(ExpandConstant('{cmd}'),
          '/C tasklist /FI "IMAGENAME eq {#AppExe}" /NH | findstr /I "{#AppExe}" >nul',
          '', SW_HIDE, ewWaitUntilTerminated, Code) then
    Result := (Code = 0);
end;

procedure CloseRunningApp();
var
  Code, I: Integer;
begin
  if not AppRunning() then
    exit;
  { Вежливое закрытие бесполезно: на WM_CLOSE приложение прячется в трей
    (on_close_requested → HideWindow), процесс живёт дальше. }
  Exec(ExpandConstant('{cmd}'), '/C taskkill /IM {#AppExe} /F /T >nul 2>&1',
       '', SW_HIDE, ewWaitUntilTerminated, Code);
  { Ждём, пока процесс реально исчезнет: подмена файла сразу после taskkill
    иногда ещё упирается в незакрытый хэндл. }
  for I := 1 to 20 do
  begin
    if not AppRunning() then
      exit;
    Sleep(250);
  end;
end;

function InitializeSetup(): Boolean;
begin
  CloseRunningApp();
  Result := True;
end;

function InitializeUninstall(): Boolean;
begin
  CloseRunningApp();
  Result := True;
end;
