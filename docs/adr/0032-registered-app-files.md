# ADR 0032: 등록 앱 파일은 플랫폼 신원과 환경 이름 원격으로 옮긴다

- 날짜: 2026-11-06
- 상태: 구현(기본 꺼짐). IAM apply 뒤에 `PLATFORM_APP_FILES=1`로 켠다. 운영 이전은 미검증

## 문제

DB 짝이 있는 등록 앱(ADR 0030)의 이전 초안에는 파일이 없었다. 앱 버킷 키는 봉인본만 있고 평문이 없어서(ADR 0016) 플랫폼이 그 키로 rclone 원격을 만들 수 없다. 허용 버킷 목록도 시작할 때 정적 카탈로그와 `PLATFORM_PLAN_BUCKETS`에서만 정해져, 새 앱 버킷은 재시작 전에는 거부됐다.

## 결정

- `PLATFORM_APP_FILES=1`일 때만 켠다. 기본은 꺼짐이고, 꺼져 있으면 지금과 똑같다.
- rclone 원격 이름은 환경 이름이다(`eks:`, `gke:`). API가 시작할 때 envs.yaml의 `storage`에서 원격을 만들어 프로세스 환경(`RCLONE_CONFIG_<ENV>_*`)에 넣는다. 엔진과 fileguard의 rclone은 이 환경을 물려받는다. 같은 원격의 `_TYPE` 변수가 이미 있으면(SSM `rclone-env`) 그 원격은 건드리지 않는다. 정적 카탈로그의 `s3:`, `gcs:` 원격과는 이름이 겹치지 않는다.
  - aws: `TYPE=s3`, `PROVIDER=AWS`, `ENV_AUTH=true`, `REGION=<region>`, `NO_CHECK_BUCKET=true`. 자격 증명은 Pod Identity다.
  - gcs: `TYPE=google cloud storage`, `ENV_AUTH=true`, `BUCKET_POLICY_ONLY=true`, `NO_CHECK_BUCKET=true`. 자격 증명은 이미 마운트된 WIF ADC(`GOOGLE_APPLICATION_CREDENTIALS=/etc/gcp/adc.json`, external_account)다.
  - minio 드라이버 환경은 원격을 만들지 않는다.
- 앱 spec에 `s3` 리소스가 있고 그 환경이 원격 대상이면 기본 인스턴스의 버킷은 `<env>:<bucketPrefix><app>`이다. 초안은 정적 앱과 같은 규칙을 따른다. 양쪽에 버킷이 있으면 파일을 옮기고, `files: false`면 옮기지 않는다.
- 정책의 허용 버킷은 plan을 parse할 때마다 짝 행에서 다시 읽어 정적 목록에 더한다(`RegistryBuckets`). 그래서 재시작 없이 새 앱 버킷이 허용되고, 앱을 지우면 다시 거부된다. 버킷 문자열 검사(`CheckBucket`)는 그대로다.
- 플랫폼이 앱 키를 평문으로 보관하는 방식은 고르지 않았다. 새 비밀 저장소가 생기고 ADR 0030의 "평문 없음"과 어긋난다. 플랫폼 신원은 이미 앱 버킷을 비우고 지울 수 있으므로 객체 읽기와 쓰기를 더해도 범위가 거의 늘지 않는다.

## 검증 방식

엔진은 `rclone check --one-way`로 원본과 대상을 비교한다. S3와 GCS의 공통 해시는 MD5다. 단일 업로드 S3 객체는 ETag, rclone이 멀티파트로 올린 객체는 `md5chksum` 메타데이터, GCS 비composite 객체는 `md5Hash`로 비교한다. 한쪽에 MD5가 없는 객체(다른 도구가 멀티파트로 올린 S3 객체, GCS composite 객체)는 크기만 비교하고 rclone이 "hashes could not be checked"로 센다. 레지던시 게이트가 켜져 있으면 fileguard는 `check --download`로 바이트를 비교하므로 제공자와 무관하다.

## 한 번 하는 설정(infra)

- AWS 플랫폼 Pod 역할: `s3:ListBucket`(`arn:aws:s3:::hopcharlie-eks-*`), `s3:GetObject`, `s3:PutObject`(`arn:aws:s3:::hopcharlie-eks-*/*`). 백업 버킷(`*---pg/*`) 객체는 Deny로 뺀다.
- GCP 플랫폼 SA의 버킷 역할(`resource.name.startsWith("projects/_/buckets/hopcharlie-gke-")` 조건)에 `storage.objects.get`, `storage.objects.create`, `storage.objects.update`를 더한다.

## 남은 것

- 허용 버킷은 앱에 묶이지 않는다. 정적 카탈로그와 같아서, 한 앱 plan이 다른 등록 앱 버킷을 가리켜도 정책은 받는다. 승인자가 요약의 files를 본다.
- 브랜치 인스턴스의 버킷은 옮기지 않는다. 초안이 기본 인스턴스만 다룬다.
