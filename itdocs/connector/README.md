# backupzit-connector

Verbindet die itdocs-Management-Plattform (Supabase) mit der BackupZit-Konsole auf demselben Server. **Nur ausgehend:** Der Connector holt Aufträge ab, führt sie über die lokale BackupZit-API aus und meldet Ergebnisse und Zustand zurück. Er öffnet keinen Port.

```
Plattform (Supabase) ◄── HTTPS, Bearer-Token, IP-Freigabe ── Connector ──► BackupZit (127.0.0.1:8443)
                                                                │
                                                                └──► Backblaze B2 (Kundenbereiche anlegen)
```

## Installation

```bash
sudo apt install ./backupzit-connector_<version>_amd64.deb
sudo nano /etc/backupzit-connector/config.json      # Werte eintragen, siehe unten
sudo -u backupzit-connector backupzit-connector -check
sudo systemctl start backupzit-connector
journalctl -u backupzit-connector -f
```

`-check` prüft alle drei Verbindungen und schreibt pro Verbindung eine Zeile mit `ok` oder `FEHLER`.

## Konfiguration (`/etc/backupzit-connector/config.json`, Rechte 0600)

| Feld | Inhalt |
|---|---|
| `platform.url` | Endpunkt der Plattform (`…/functions/v1/backupzit-connector`) |
| `platform.token` | `BACKUPZIT_CONNECTOR_TOKEN` aus den Supabase-Secrets |
| `backupzit.token` | API-Token des Benutzers `svc-connector` (Rolle Administrator) |
| `backupzit.fingerprint` | Fingerabdruck des Konsolen-Zertifikats: `journalctl -u backupzit-server \| grep -o 'fingerprint=[^ ]*'` |
| `b2.key_id`, `b2.app_key` | Verwaltungsschlüssel für Backblaze: darf Buckets auflisten sowie Schlüssel anlegen und löschen |
| `b2.bucket` | gemeinsamer Backup-Bucket (privat, Object Lock aktiv) |
| `b2.lock_days` | Unveränderlichkeit neuer Kundenspeicher in Tagen (Standard 30) |

Geheimnisse können auch per Umgebungsvariable kommen: `BZC_PLATFORM_TOKEN`, `BZC_BACKUPZIT_TOKEN`, `BZC_B2_KEY_ID`, `BZC_B2_APP_KEY`.

## Kundenspeicher (`target.provision`)

Pro Kunde:
1. Ein Ordner `c-<kunden-uuid>/` im gemeinsamen Bucket. Der Ordnername enthält keinen Kundennamen.
2. Ein eigener B2-Schlüssel, beschränkt auf genau diesen Bucket und Ordner. Er darf nur Dateien auflisten, lesen, schreiben und löschen sowie Object-Lock-Fristen setzen. Er darf **keine** Buckets ändern, **keine** Schlüssel anlegen und Object Lock **nicht** umgehen.
3. Ein BackupZit-Speicherziel mit diesem Schlüssel: verschlüsselt, Object Lock (`lock_days`).

Der Kundenschlüssel bleibt in BackupZit und geht nicht an die Plattform. Die Plattform bekommt nur die Speicherziel-ID und einmalig den Wiederherstellungsschlüssel. Schlägt Schritt 3 fehl, löscht der Connector den gerade angelegten Schlüssel wieder. Ein zweiter Speicher für denselben Kunden wird abgelehnt.

## Auftragsarten

`target.provision`, `target.create`, `target.test`, `target.update`, `agent.enroll_code`, `agent.installer`, `agent.log`, `agent.browse`, `job.create`, `job.update`, `job.delete`, `job.run`, `job.restore_test`, `job.enable`, `job.disable`, `run.cancel`, `files.list`, `files.download`, `restore.to_folder`, `restore.files_original`, `restore.machine`, `restore.volumes`, `recovery.code`, `recovery.revoke`.

- **Lokale und Netzwerkziele** (`destination` bei `job.create`): Der Connector legt ein BackupZit-Ziel an, verschlüsselt mit dem Wiederherstellungsschlüssel des Cloud-Speichers des Kunden (`recovery_key_from`). Der Cloud-Speicher muss also zuerst bestehen. Derselbe Pfad wird wiederverwendet; bei einer Freigabe wird dabei das übergebene Kennwort gespeichert. Das Ziel steht als `destination_target` im Ergebnis.
- **`ext`-Felder**: Monate, Zeitraum, Wochentag im Monat, Zeitfenster mit Minuten und „Stunden seit der letzten Sicherung“ werden zu BackupZit-Zeitplänen. Aufbewahrung nach Alter wird in Anzahlen umgerechnet (z. B. 90 Tage → 90 tägliche). Gesperrte und gedrosselte Stunden des Leistungsfensters werden zu Sicherungsfenster und Bandbreitengrenze, wenn sie für alle Tage gleich und zusammenhängend sind. Was BackupZit nicht kann, kommt als `warnings` zurück.
- **`ext_disks`**: eine Platte → diese Platte (ganz oder Partitionen); mehrere → „alle internen Datenträger“.
- **`agent.browse`**: nur die Laufwerke aus dem Datenträgerbericht des Agenten; Ordner darunter kann BackupZit nicht auflisten.
- **`restore.machine` / `restore.volumes`**: je Zuordnung eine Wiederherstellung auf eine ganze Platte (`POST /runs/{id}/restore-disk`, Bestätigung des Löschens durch die Plattform). Einzelne Volumes ohne den Rest der Platte gehen nicht.
- **`recovery.code`**: `recovery.json` für den Wiederherstellungsstick, standardmäßig 24 Stunden gültig, für mehrere Starts; `recovery.revoke` widerruft die Codes des Kunden (oder eines Geräts).

## Agent-Installation (`agent.installer`)

Nach der Freigabe durch einen Techniker auf dem Mesh-Server stellt die Plattform den Auftrag `agent.installer` ein. Der Connector:
1. erzeugt einen **Einmal-Enrollment-Code**, 24 Stunden gültig, mit der Kennung `itdocs:<customer_id>:<device_id>`. Der Agent, der sich damit anmeldet, trägt diese Kennung als `enroll_label`, und die Plattform ordnet ihn automatisch dem Gerät zu. Ein zweiter Agent kann mit demselben Code nicht registriert werden.
2. nimmt das **offizielle, signierte Agentenpaket** aus `/usr/share/backupzit/dist`: Windows als MSI, Linux als deb, bei RHEL-/Alma-/Rocky-/SUSE-Systemen als rpm. Es lädt das Paket über eine signierte URL zur Plattform hoch.
3. meldet pro Paket `sha256`, `size_bytes`, `version`, `package_type` und die Aufrufparameter zurück:
   - **msi:** `msiexec /i <datei> <install_args>`, mit `install_args` = `/qn /norestart ENROLLCODE=BZ1-…`
   - **deb/rpm:** Paket mit dem Paketmanager installieren, danach die Befehle aus `post_install` ausführen (`backupzit-agent enroll --if-not-enrolled --pending-ok --code BZ1-…`, `systemctl restart backupzit-agent`)
   - **Erfolgskontrolle:** `service_name` (immer `backupzit-agent`, unter Windows der Dienst „BackupZit Agent“, unter Linux die systemd-Unit) muss danach laufen.

Der Code in `install_args` bzw. `post_install` ist ein Geheimnis: nicht protokollieren und nach der Installation löschen.

## Tests

```bash
go test ./...                       # mit simulierter Plattform, B2 und Konsole
go test -tags e2e -run E2E -v ./... # gegen eine echte Konsole, siehe e2e_test.go
```
