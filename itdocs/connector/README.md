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

`target.provision`, `target.create`, `target.test`, `agent.enroll_code`, `agent.log`, `job.create`, `job.update`, `job.delete`, `job.run`, `job.restore_test`, `job.enable`, `job.disable`, `run.cancel`, `files.list`, `files.download`, `restore.to_folder`.

Angenommen, aber noch abgelehnt mit verständlicher Meldung: `agent.browse`, `restore.files_original`, `restore.machine`, `restore.volumes`.

Funktionen, die BackupZit noch nicht kann, etwa erweiterte Zeitpläne, Aufbewahrung nach Alter oder ein Kennwort pro Job, werden bei `job.create` und `job.update` ignoriert. Sie kommen als `warnings` im Ergebnis zurück. Lokale und Netzwerkziele lehnt der Connector ab.

## Tests

```bash
go test ./...                       # mit simulierter Plattform, B2 und Konsole
go test -tags e2e -run E2E -v ./... # gegen eine echte Konsole, siehe e2e_test.go
```
