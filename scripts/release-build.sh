#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'Usage: %s --intent ABS_INTENT_JSON --source-dir SOURCE_DIR --output-dir NEW_PRIVATE_OUTPUT_DIR\n' "$0" >&2
  exit 2
}

intent=''
source_dir=''
output_dir=''
while (($#)); do
  case "$1" in
    --intent|--source-dir|--output-dir)
      (($# >= 2)) || usage
      case "$1" in
        --intent) intent=$2 ;;
        --source-dir) source_dir=$2 ;;
        --output-dir) output_dir=$2 ;;
      esac
      shift 2
      ;;
    *) usage ;;
  esac
done
[[ -n "$intent" && -n "$source_dir" && -n "$output_dir" ]] || usage
[[ "$intent" = /* && "$source_dir" = /* && "$output_dir" = /* ]] || { echo 'All paths must be absolute' >&2; exit 2; }
[[ -f "$intent" && ! -L "$intent" ]] || { echo 'Intent must be a regular file' >&2; exit 1; }
[[ -d "$source_dir" && ! -L "$source_dir" ]] || { echo 'Source directory must be a real directory' >&2; exit 1; }
[[ -d "$output_dir" && ! -L "$output_dir" ]] || { echo 'Output directory must be a newly created real directory' >&2; exit 1; }
chmod 700 "$output_dir"
[[ "$(cd -- "$(dirname -- "$intent")" && pwd -P)" == "$(cd -- "$output_dir" && pwd -P)" ]] || { echo 'Intent must be inside the private output directory' >&2; exit 1; }
[[ -f "$intent" && ! -L "$intent" ]] || { echo 'Intent must be a regular file' >&2; exit 1; }
for entry in "$output_dir"/* "$output_dir"/.[!.]* "$output_dir"/..?*; do
  [[ -e "$entry" ]] || continue
  [[ "$entry" == "$intent" ]] || { echo 'Output directory must contain only the intent file' >&2; exit 1; }
done

# Read only the expected, validated fields. Never evaluate intent content as shell.
fields_text=$(python3 - "$intent" <<'PY'
import json, re, sys
with open(sys.argv[1], encoding='utf-8') as f:
    d = json.load(f)
expected = {'version', 'repository', 'commit', 'platform', 'publish_requested', 'previous_image', 'run_id', 'run_attempt', 'image_tag'}
if set(d) != expected:
    raise SystemExit('intent fields do not match contract')
if d['version'] != 1 or not isinstance(d['repository'], str) or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', d['repository']):
    raise SystemExit('invalid intent repository/version')
if not isinstance(d['commit'], str) or not re.fullmatch(r'[0-9a-f]{40}', d['commit']):
    raise SystemExit('invalid intent commit')
if d['platform'] not in ('linux/amd64', 'linux/arm64') or type(d['publish_requested']) is not bool:
    raise SystemExit('invalid intent platform/publish flag')
if d['previous_image'] is not None and not isinstance(d['previous_image'], str):
    raise SystemExit('invalid previous image')
for k in ('run_id', 'run_attempt'):
    if not isinstance(d[k], (str, int)) or not re.fullmatch(r'[0-9]+', str(d[k])):
        raise SystemExit('invalid run metadata')
want = f"ghcr.io/{d['repository'].lower()}:git-{d['commit']}-run-{d['run_id']}-{d['run_attempt']}"
if d['image_tag'] != want:
    raise SystemExit('invalid image tag')
for value in (d['repository'], d['commit'], d['platform'], 'true' if d['publish_requested'] else 'false', d['previous_image'] or '', d['image_tag'], str(d['run_id']), str(d['run_attempt'])):
    print(value)
PY
)
fields=()
while IFS= read -r value; do fields[${#fields[@]}]=$value; done <<< "$fields_text"
((${#fields[@]} == 8)) || { echo 'Could not parse validated intent' >&2; exit 1; }
repository=${fields[0]}
commit=${fields[1]}
platform=${fields[2]}
publish=${fields[3]}
previous_image=${fields[4]}
tag=${fields[5]}
intent_run_id=${fields[6]}
intent_run_attempt=${fields[7]}

for name in GITHUB_ACTIONS GITHUB_REF GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT GH_TOKEN; do
  [[ -n "${!name:-}" ]] || { printf 'Missing canonical CI variable: %s\n' "$name" >&2; exit 1; }
done
[[ "$GITHUB_ACTIONS" == true && "$GITHUB_REPOSITORY" == "$repository" ]] || { echo 'Intent does not match canonical Actions repository' >&2; exit 1; }
[[ "$GITHUB_RUN_ID" =~ ^[0-9]+$ && "$GITHUB_RUN_ATTEMPT" =~ ^[0-9]+$ ]] || { echo 'Invalid canonical run metadata' >&2; exit 1; }
if [[ "$publish" == true ]]; then
  [[ "$GITHUB_RUN_ATTEMPT" == 1 ]] || { echo 'Publishing is not supported on workflow reruns; start a new workflow run' >&2; exit 1; }
fi
[[ "$intent_run_id" == "$GITHUB_RUN_ID" && "$intent_run_attempt" == "$GITHUB_RUN_ATTEMPT" ]] || { echo 'Intent run metadata does not match canonical Actions context' >&2; exit 1; }
if [[ "$publish" == true ]]; then
  for name in GITHUB_ACTOR GITHUB_ACTOR_ID GITHUB_TOKEN; do
    [[ -n "${!name:-}" ]] || { printf 'Missing protected publish variable: %s\n' "$name" >&2; exit 1; }
  done
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
preflight="$script_dir/release-preflight.py"
manifest="$script_dir/release-manifest.py"
[[ -f "$preflight" && -f "$manifest" ]] || { echo 'Trusted release helper is missing' >&2; exit 1; }

preflight_count=0
fresh=''
run_preflight() {
  preflight_count=$((preflight_count + 1))
  ((preflight_count <= 10)) || { echo 'Too many fresh preflight attempts' >&2; return 1; }
  fresh="$output_dir/fresh-intent-$preflight_count.json"
  [[ ! -e "$fresh" && ! -L "$fresh" ]] || { echo 'Fresh preflight output path already exists' >&2; return 1; }
  if [[ "$publish" == true ]]; then
    python3 "$preflight" --commit "$commit" --repository "$repository" --platform "$platform" --publish "$publish" --confirm "${RELEASE_CONFIRM:-}" --previous-image "$previous_image" --require-approval --output "$fresh"
  else
    python3 "$preflight" --commit "$commit" --repository "$repository" --platform "$platform" --publish "$publish" --confirm "${RELEASE_CONFIRM:-}" --previous-image "$previous_image" --output "$fresh"
  fi
  python3 - "$intent" "$fresh" <<'PY'
import json, sys
keys = ('version', 'repository', 'commit', 'platform', 'publish_requested', 'previous_image', 'run_id', 'run_attempt', 'image_tag')
with open(sys.argv[1], encoding='utf-8') as f: old = json.load(f)
with open(sys.argv[2], encoding='utf-8') as f: new = json.load(f)
if any(old.get(k) != new.get(k) for k in keys):
    raise SystemExit('Fresh preflight intent differs from saved intent')
PY
}

# Fresh preflight is mandatory before any Docker operation. Source metadata is also checked first.
run_preflight
head=$(git -C "$source_dir" rev-parse --verify HEAD^{commit})
[[ "$head" == "$commit" ]] || { echo 'Source checkout HEAD does not match validated commit' >&2; exit 1; }
[[ -z "$(git -C "$source_dir" status --porcelain --untracked-files=no)" ]] || { echo 'Source checkout has tracked changes' >&2; exit 1; }
[[ -f "$source_dir/Dockerfile" && ! -L "$source_dir/Dockerfile" ]] || { echo 'Source Dockerfile is missing or unsafe' >&2; exit 1; }

image_config="$output_dir/image-config.json"
archive="$output_dir/image.tar"
manifest_out="$output_dir/release-manifest.json"
local_manifest="$output_dir/local-validation-manifest.json"
docker buildx build --platform "$platform" --build-arg "VCS_REF=$commit" --load --tag "$tag" "$source_dir"
docker image inspect "$tag" > "$image_config"
docker image save --output "$archive" "$tag"
if [[ "$publish" == false ]]; then
  python3 "$manifest" --intent "$fresh" --source-dir "$source_dir" --image-config "$image_config" --archive "$archive" --digest '' --published false --output "$manifest_out"
  exit 0
fi
python3 "$manifest" --intent "$fresh" --source-dir "$source_dir" --image-config "$image_config" --archive "$archive" --digest '' --published false --output "$local_manifest"

# Recheck protected-environment approval, exact CI state and unchanged intent immediately before credentials/push.
run_preflight
private_docker_config="$output_dir/.docker-config"
mkdir -m 700 "$private_docker_config"
export DOCKER_CONFIG="$private_docker_config"
logged_in=false
cleanup() {
  local original_status=$?
  trap - EXIT
  if [[ "$logged_in" == true ]]; then docker logout ghcr.io >/dev/null 2>&1 || true; fi
  if ! rm -rf -- "$private_docker_config"; then
    echo 'Failed to remove private Docker configuration directory' >&2
    [[ "$original_status" -ne 0 ]] || original_status=1
  fi
  exit "$original_status"
}
trap cleanup EXIT
printf '%s' "$GITHUB_TOKEN" | docker login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
logged_in=true
docker push "$tag"
repo_digests=$(docker image inspect --format '{{json .RepoDigests}}' "$tag")
digest_ref=$(python3 - "$repo_digests" "$repository" <<'PY'
import json, re, sys
values = json.loads(sys.argv[1])
repo = 'ghcr.io/' + sys.argv[2].lower()
if not isinstance(values, list):
    raise SystemExit('Docker returned invalid RepoDigests')
matches = [v for v in values if isinstance(v, str) and re.fullmatch(re.escape(repo) + r'@sha256:[0-9a-f]{64}', v)]
if len(matches) != 1:
    raise SystemExit('Could not identify one exact pushed registry digest')
print(matches[0])
PY
)
python3 "$manifest" --intent "$fresh" --source-dir "$source_dir" --image-config "$image_config" --archive "$archive" --digest "$digest_ref" --published true --output "$manifest_out"
