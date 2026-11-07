# 0034. 콜드 파일 검사에서 디스크 작업 목록과 제한된 일괄 다운로드를 사용한다

- 상태: 채택
- 날짜: 2026-11-06
- 관련: platform #87, ADR 0031

## 상황

검사 이력 없는 최초 실행은 원본 전체 본문을 읽어야 한다. 기존 경로는 전체 lsjson과 객체 배열/manifest를 메모리에 보관하고 객체마다 rclone cat 프로세스를 시작했다. 같은 로컬 MinIO의 작은 텍스트 32개에서 전체 약 12~13초 중 다운로드가 약 11초였고 프로세스는 총 35개였다. 캐시 도입이나 제한 상향으로 해결할 문제가 아니다.

## 결정

- lsjson을 크기 제한 pipe로 streaming decode한다. 전체 열거 성공과 닫힌 JSON 배열/프로세스 성공이 모두 필요하다. 기존 목록 출력 8 MiB, 객체 10,000개·객체당 16 MiB·본문 총 256 MiB 제한을 유지한다.
- private TEMP root(Unix 0700)에 256개 단위 정렬 run과 heap merge를 사용한다. Unicode simple-fold 충돌을 먼저 검사한 뒤 기존 Path 순서로 정렬한다. 고정 chunk와 run당 한 레코드만 메모리에 유지한다. metadata 파일과 원본 키는 외부로 전송하지 않는다.
- 작업 목록을 SHA-256으로 seal하고 매 순회에서 크기·개수·안전한 경로와 바이트 digest를 다시 확인한다. 파일 존재나 완료 이름을 재개 허가로 사용하지 않는다.
- 최대 256개 literal files-from-raw window마다 rclone copy를 사용한다. 전송/검사기 각 4개, read buffer 각 1 MiB, backlog 256, multi-thread streams 0으로 제한한다. 따라서 rclone filter에도 전체 객체 목록을 누적하지 않는다. 남은 staging 바이트 예산을 hard max-transfer로 적용하고 매 window의 실제 파일 형식·크기를 확인한다. source가 변경되어 커진 파일은 미검사 상태로 업로드되지 않는다.
- files-from-raw와 max-size filter는 rclone에서 양립하지 않는다. source 목록의 16 MiB 제한, 다운로드 후 정확한 크기 대조 및 readObject의 16 MiB 제한으로 검사 범위를 유지하고, 수신 중의 변경은 남은 총 256 MiB hard budget으로 제한한다. 전송 오류·잘린 파일·disk 부족은 전체 실패다. 정확히 hard limit에 도달한 작업이 rclone에 의해 거절될 수도 있으며 fail-closed를 우선한다.
- 다운로드 뒤 두 번째 전체 열거의 정규화 목록 digest를 비교한다. 추가·삭제·크기 변경을 발견하면 중단한다. 원자적 원격 snapshot이나 writer fence를 제공하지 않으며 같은 크기 내용 변경도 언제나 검출한다고 주장하지 않는다. 실제 검사한 staging만 전송한다.
- 검사한 각 본문에서 review lookup과 manifest용 SHA-256을 공유한다. 검사 manifest는 기존 JSON 형식 그대로 hash에 streaming하고 private inspected.tmp에도 쓴다. 전체 성공 뒤 inspected.json으로 rename한다. 규칙·키·해시·분류는 내부 manifest에만 있고 외부 Report는 기존 aggregate와 결정적인 digest를 유지한다.
- 정책 완료 전에 target sync는 없다. 이후 기존 staging 재검사·digest 비교를 유지하고, target check --download --one-way도 유지한다. ZIP의 누적 팽창 64 MiB·멤버/깊이 예산, 지원 형식과 review 의미를 바꾸지 않는다.

## 검토한 대안

- cat 프로세스만 병렬화: 프로세스와 연결 초기화가 여전히 객체 수에 비례하고 메모리도 여러 프로세스만큼 늘어난다.
- 전체 bucket 무제한 copy: 목록 변경·용량과 경로 통제가 약해져 기각했다.
- files-from 전체를 한 프로세스에 전달: subprocess는 적지만 rclone filter의 전체 목록 메모리를 피하지 못해 256개 window를 택했다.
- 지속 rclone HTTP/RC 서버: 관리 listener/자격증명/수명·인증이라는 새로운 실행 경계가 필요해 도입하지 않았다.
- SQLite/검사 이력 DB: 이 단계는 재사용 캐시가 아닌 한 실행의 목록 관리다. 기존 의존성 없이 외부 정렬 파일로 충분하다.
- staging 재검사 삭제·부분 검사 후 먼저 업로드·블룸 필터: byte binding과 전체 이동 단위 정책을 훼손해 기각했다.

## 결과

32개 데이터셋의 다운로드 프로세스는 32→1, 전체 프로세스는 35→5다. 10,000개이면 최대 40개의 download window이며 원격 요청 수 감소는 보장하지 않는다. 실제 MinIO 측정에서 추가 전체 열거 때문에 작은 파일의 요청 수는 230→233으로 소폭 늘었다. 성능·RSS·전송 바이트·10ms sampling staging 사용량은 test-records에 기록한다. inspection history는 매번 비어 있고 OS/backend cache는 flush하지 않았다. RSS는 Go parent와 최대 개별 child를 각각 측정하며 동시 전체 시스템 RSS가 아니다. scan/hash/manifest와 staging verify는 합산 구간으로 측정한다.

메모리 본문은 기존 객체/ZIP 예산을 유지하며 metadata는 디스크로 이동한다. manifest당 16 MiB 상한과 두 listing의 외부 정렬·inspection 임시 파일을 고려해 별도 metadata 디스크 여유가 필요하다. 본문 256 MiB 외에 metadata 최대 약 64 MiB와 filesystem block/inode, process/buffer 여유를 private TEMP에 확보해야 한다. 정상 오류/취소는 root와 partial 파일을 지운다. SIGKILL/전원 장애에는 defer가 실행되지 않으므로 서비스 계정만 접근 가능한 TEMP/ephemeral volume의 잔여물 정리가 필요하다. 재개·검사 이력 재사용은 지원하지 않으며 남은 디렉터리는 운영자가 활성 작업과 구분해 정리해야 한다.

활발히 객체를 추가/삭제하는 source는 새 안정성 비교에서 실패할 수 있다. 이 경우 caller가 새 실행을 시작해야 하며 자동 snapshot/fencing을 가정하지 않는다. 실제 local rclone 및 MinIO sandbox와 1 MiB tmpfs 용량 부족을 검증한다. 실제 AWS S3/GCS와 운영 버킷은 사용하지 않았다. metadata/tags/ACL/버전/Content-Type 보존, OCR/DLP 확장, cross-environment 복구 및 동시 원격 쓰기의 정합성 보장은 별도 문제다. 기존 KR 검사 위치·서버 소유 자격증명 및 비식별 오류 경계를 유지한다. 로컬 sandbox 제거 외에 운영 변경과 비용은 없다.
