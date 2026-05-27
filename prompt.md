# Projekt: Nextcloud App Store Relay (DMZ-tauglich)

Ich möchte einen schlanken, wartungsarmen Relay/Proxy als Docker-Container bauen, der als Vermittler zwischen einer abgeschotteten Nextcloud-Instanz und dem offiziellen Nextcloud App Store (`https://apps.nextcloud.com/api/v1`) dient. Meine Nextcloud hat keinen direkten Internetzugang; der Relay steht in der DMZ und darf nach außen. Über die `appstoreurl`-Option in der Nextcloud-`config.php` zeige ich auf diesen Relay, und darüber sollen Apps (Talk/spreed, Calendar, Contacts etc.) ganz normal installiert und aktualisiert werden können.

## Wie der Nextcloud App Store funktioniert (wichtig fürs Verständnis)

- Nextcloud ruft am konfigurierten `appstoreurl` zwei JSON-Endpunkte ab: den App-Katalog (`apps.json` bzw. der entsprechende API-Pfad) und die Kategorien (`categories.json`).
- Die App-Metadaten in dieser JSON enthalten pro App-Release eine **Download-URL** für das Tarball (`.tar.gz`). Diese URLs zeigen NICHT auf apps.nextcloud.com, sondern typischerweise auf externe Server, meist `github.com/nextcloud-releases/<app>/releases/download/...`.
- Die App-Pakete sind **code-signiert**. Die JSON enthält pro Release zusätzlich eine Signatur und einen Hash, die Nextcloud nach dem Download gegen das Tarball prüft.

## Die zwei nicht verhandelbaren Anforderungen

1. **URL-Rewriting der Download-Links:** Der Relay muss die Download-URLs in den JSON-Antworten so umschreiben, dass sie auf den Relay selbst zeigen (z. B. `https://relay.example/download/<app>/<version>`). Wenn Nextcloud diese umgeschriebene URL dann abruft, holt der Relay das Original-Tarball vom echten Ziel (GitHub etc.) und streamt es durch. Andernfalls findet Nextcloud die Apps im Katalog, kann sie aber nicht laden.

2. **Byte-genaue Durchleitung der Tarballs und Signaturdaten:** Die Tarballs dürfen auf dem Weg durch den Relay in KEINER Weise verändert werden — kein Re-Encoding, keine Dekompression/Rekompression, keine Modifikation. Auch die signatur- und hash-relevanten Felder in der JSON dürfen NICHT verändert werden (außer den Download-URLs selbst). Sonst schlägt die Signaturprüfung in Nextcloud fehl. Das URL-Rewriting in der JSON muss so minimal-invasiv wie möglich sein; idealerweise nur die URL-Strings ersetzen, ohne die JSON komplett neu zu serialisieren, falls das die Byte-Struktur signaturrelevanter Teile berührt. Prüfe und dokumentiere explizit, ob Signatur/Hash sich auf das Tarball beziehen (dann ist JSON-Manipulation unkritisch) oder auch auf JSON-Felder.

## Weitere Anforderungen

- **Caching:** Bereits geladene Tarballs UND die JSON-Antworten lokal cachen (Dateisystem-Cache reicht), mit konfigurierbarer TTL für die JSON. Tarballs sind unveränderlich pro Version und können dauerhaft gecacht werden. Wiederholte Abrufe sollen nicht erneut nach außen gehen.
- **Wartungsarm:** So wenig bewegliche Teile wie möglich. Bevorzuge eine kleine, gut lesbare Anwendung (Python/FastAPI oder Go) ODER eine nginx/Caddy-basierte Lösung mit Rewriting — wähle die robustere Variante und begründe die Wahl kurz. KEINE schwergewichtige Infrastruktur (kein Django/PostgreSQL).
- **Resilienz:** Wenn das Upstream (apps.nextcloud.com / GitHub) nicht erreichbar ist, soll der Relay aus dem Cache antworten, statt komplett auszufallen.
- **Konfigurierbarkeit:** Upstream-URL, Cache-Pfad, TTL, eigene öffentliche Relay-URL (für das Rewriting) über Umgebungsvariablen.
- **Logging:** Nachvollziehbar, welche App-Abrufe und Downloads über den Relay gelaufen sind, inkl. Cache-Hit/Miss.

## Was ich als Ergebnis möchte

- Vollständiger, lauffähiger Code in einem sauberen Repo-Layout (öffentlich-tauglich, mit sinnvollem `README.md` und Lizenzhinweis-Platzhalter).
- `Dockerfile` und `docker-compose.yml`.
- Konfiguration über `.env` mit Beispiel-`.env.example`.
- Die exakte `occ`-Zeile UND der alternative `config.php`-Eintrag, um `appstoreurl` meiner Nextcloud auf den Relay zu setzen.
- Eine **Testanleitung**: wie ich (a) prüfe, ob der Katalog beim Relay korrekt ankommt, (b) eine Beispiel-App über den Relay installiere, (c) ein Update durchspiele, und (d) verifiziere, dass die Signaturprüfung durchläuft.
- Eine kurze, ehrliche `LIMITATIONS.md`: woran die Lösung scheitern kann (z. B. wenn Nextcloud das API-Schema ändert), und woran man so einen Bruch erkennt.

## Vorgehen

Bevor du Code schreibst: Inspiziere zuerst die echten API-Antworten von `https://apps.nextcloud.com/api/v1/apps.json` (oder dem aktuellen Pfad) und schau dir die reale Struktur der Download-URLs und der Signaturfelder an. Verifiziere damit die obigen Annahmen, statt sie blind zu übernehmen — falls die Realität abweicht, passe das Design an und sag mir, was anders ist. Erst danach implementieren.
