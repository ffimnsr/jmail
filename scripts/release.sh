#!/usr/bin/env bash

set -euo pipefail

readonly REMOTE_NAME="origin"
readonly CHANGELOG_FILE="CHANGELOG.md"
readonly PACKAGE_SCRIPT="package.sh"
readonly CONTAINERFILE="Containerfile"

usage() {
  cat <<'EOF'
Usage: scripts/release.sh [options] [<version>]

Run the release gates, write a CHANGELOG.md section from the commits since the previous
release, create a "release: vX.Y.Z" commit, tag it, and optionally push commit and tag.

Pushing the tag is what starts the image build (build-push-release-images.yml) and the
documentation deploy (build-deploy-pages.yml).

Version source of truth: the newest vX.Y.Z git tag.

Options:
  --major        Increment major version and reset minor/patch to zero.
  --minor        Increment minor version and reset patch to zero.
  --patch        Increment patch version.
  --skip-build   Skip the local container image build.
  --skip-push    Skip pushing the release commit and tag to origin.
  -h, --help     Show this help message.

Examples:
  scripts/release.sh --patch
  scripts/release.sh --minor --skip-push
  scripts/release.sh 1.1.0
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

append_section_entry() {
  local section_name="$1"
  local entry="$2"

  printf -v "$section_name" '%s- %s\n' "${!section_name}" "$entry"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

ensure_clean_worktree() {
  git diff --quiet --exit-code || die "working tree has unstaged changes"
  git diff --cached --quiet --exit-code || die "index has staged but uncommitted changes"
}

previous_release_tag() {
  # Match the current vX.Y.Z scheme and the legacy release-X.Y.Z one, so the first
  # semver release continues the existing version sequence instead of starting at 0.
  git describe --tags --abbrev=0 \
    --match 'v[0-9]*.[0-9]*.[0-9]*' \
    --match 'release-[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true
}

current_version() {
  local tag
  tag="$(previous_release_tag)"
  tag="${tag#v}"
  tag="${tag#release-}"
  printf '%s\n' "$tag"
}

increment_version() {
  local current="$1"
  local bump_kind="$2"
  local major minor patch

  IFS='.' read -r major minor patch <<<"$current"

  case "$bump_kind" in
    major)
      major=$((major + 1))
      minor=0
      patch=0
      ;;
    minor)
      minor=$((minor + 1))
      patch=0
      ;;
    patch)
      patch=$((patch + 1))
      ;;
    *)
      die "unsupported bump kind: $bump_kind"
      ;;
  esac

  printf '%s.%s.%s\n' "$major" "$minor" "$patch"
}

# Commits in this repository are prefixed with an emoji ("🍕 feat: ..."), which would
# otherwise put every entry in "Other Changes".
strip_decoration() {
  local subject="$1"

  if [[ "$subject" =~ ^[^[:alnum:]]+[[:space:]]+ ]]; then
    subject="${subject:${#BASH_REMATCH[0]}}"
  fi

  printf '%s\n' "$subject"
}

render_changelog_group() {
  local title="$1"
  local content="$2"

  [[ -n "$content" ]] || return 0

  printf '### %s\n\n' "$title"
  # $content already ends with a newline; adding one here keeps a single blank line
  # between groups.
  printf '%s' "$content"
  printf '\n'
}

update_changelog() {
  local version="$1"
  local release_date="$2"
  local previous_tag="$3"
  local log_range
  local features=""
  local fixes=""
  local docs=""
  local tests=""
  local ci=""
  local maintenance=""
  local other=""
  local entry_count=0
  local conventional_commit_regex='^([[:alnum:]_-]+)(\([^)]+\))?(!)?:[[:space:]]*(.+)$'

  if [[ -n "$previous_tag" ]]; then
    log_range="${previous_tag}..HEAD"
  else
    log_range="HEAD"
  fi

  while IFS=$'\t' read -r commit_sha subject; do
    local short_sha category message commit_type
    [[ -n "$commit_sha" ]] || continue

    short_sha="$(git rev-parse --short "$commit_sha")"
    category="other"
    subject="$(strip_decoration "$subject")"
    message="$subject"

    if [[ "$subject" =~ $conventional_commit_regex ]]; then
      commit_type="${BASH_REMATCH[1]}"
      message="${BASH_REMATCH[4]}"

      case "$commit_type" in
        feat)
          category="features"
          ;;
        fix)
          category="fixes"
          ;;
        docs)
          category="docs"
          ;;
        test|tests)
          category="tests"
          ;;
        ci)
          category="ci"
          ;;
        build|style|refactor|perf|chore)
          category="maintenance"
          ;;
      esac
    fi

    case "$category" in
      features)
        append_section_entry features "${message} (\`${short_sha}\`)"
        ;;
      fixes)
        append_section_entry fixes "${message} (\`${short_sha}\`)"
        ;;
      docs)
        append_section_entry docs "${message} (\`${short_sha}\`)"
        ;;
      tests)
        append_section_entry tests "${message} (\`${short_sha}\`)"
        ;;
      ci)
        append_section_entry ci "${message} (\`${short_sha}\`)"
        ;;
      maintenance)
        append_section_entry maintenance "${message} (\`${short_sha}\`)"
        ;;
      *)
        append_section_entry other "${message} (\`${short_sha}\`)"
        ;;
    esac

    entry_count=$((entry_count + 1))
  done < <(git log --reverse --format='%H%x09%s' "$log_range")

  ((entry_count > 0)) || die "no commits found for changelog range: ${log_range}"

  if [[ -f "$CHANGELOG_FILE" ]] && grep -Eq "^## ${version//./\\.}([[:space:]]|$)" "$CHANGELOG_FILE"; then
    die "$CHANGELOG_FILE already contains an entry for version $version"
  fi

  local tmp preface existing_releases
  tmp="$(mktemp)"
  preface=""
  existing_releases=""

  if [[ -f "$CHANGELOG_FILE" ]]; then
    preface="$(awk '
      BEGIN { seen_release = 0 }
      /^## [0-9]+\.[0-9]+\.[0-9]+([[:space:]]-|$)/ {
        seen_release = 1
        exit
      }
      { print }
    ' "$CHANGELOG_FILE")"

    existing_releases="$(awk '
      BEGIN { seen_release = 0 }
      /^## [0-9]+\.[0-9]+\.[0-9]+([[:space:]]-|$)/ {
        seen_release = 1
      }
      seen_release { print }
    ' "$CHANGELOG_FILE")"
  fi

  {
    if [[ -n "$preface" ]]; then
      printf '%s\n' "$preface"
      [[ "$preface" == *$'\n' ]] || printf '\n'
    else
      printf '# Changelog\n\n'
    fi

    printf '## %s - %s\n\n' "$version" "$release_date"
    render_changelog_group "Features" "$features"
    render_changelog_group "Fixes" "$fixes"
    render_changelog_group "Documentation" "$docs"
    render_changelog_group "Tests" "$tests"
    render_changelog_group "CI" "$ci"
    render_changelog_group "Maintenance" "$maintenance"
    render_changelog_group "Other Changes" "$other"

    if [[ -n "$existing_releases" ]]; then
      printf '%s\n' "$existing_releases"
    fi
  } >"$tmp"

  chmod 0644 "$tmp"
  mv "$tmp" "$CHANGELOG_FILE"
}

check_shell_syntax() {
  local script
  local failed=0

  while IFS= read -r script; do
    bash -n "$script" || failed=1
  done < <(git ls-files '*.sh' 'data/s6-overlay/s6-rc.d/*/run')

  ((failed == 0)) || die "shell syntax check failed"
}

check_docs() {
  local output_dir

  if ! command -v mdbook >/dev/null 2>&1; then
    printf 'mdbook not installed, skipping documentation build\n'
    return 0
  fi

  output_dir="$(mktemp -d)"
  mdbook build -d "$output_dir" docs >/dev/null || die "documentation build failed"
  rm -rf "$output_dir"
}

build_image() {
  [[ -f "$PACKAGE_SCRIPT" ]] || die "missing $PACKAGE_SCRIPT"

  printf 'Building the image as version %s...\n' "$1"
  VERSION="$1" bash "$PACKAGE_SCRIPT"
}

ensure_tag_absent() {
  local tag_name="$1"

  git rev-parse --verify "refs/tags/$tag_name" >/dev/null 2>&1 &&
    die "tag '$tag_name' already exists locally"

  if git remote get-url "$REMOTE_NAME" >/dev/null 2>&1; then
    git ls-remote --exit-code --tags "$REMOTE_NAME" "refs/tags/$tag_name" >/dev/null 2>&1 &&
      die "tag '$tag_name' already exists on '$REMOTE_NAME'" || true
  fi
}

ensure_remote_exists() {
  git remote get-url "$REMOTE_NAME" >/dev/null 2>&1 || die "git remote '$REMOTE_NAME' is not configured"
}

main() {
  local run_push=1
  local run_build=1
  local version=""
  local bump_kind=""

  while (($# > 0)); do
    case "$1" in
      --major|--minor|--patch)
        [[ -z "$bump_kind" ]] || die "only one of --major, --minor, or --patch may be used"
        bump_kind="${1#--}"
        shift
        ;;
      --skip-build)
        run_build=0
        shift
        ;;
      --skip-push)
        run_push=0
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      -*)
        die "unknown option: $1"
        ;;
      *)
        [[ -z "$version" ]] || die "version may only be provided once"
        version="$1"
        shift
        ;;
    esac
  done

  if [[ -z "$version" && -z "$bump_kind" ]]; then
    usage
    exit 1
  fi

  [[ -z "$version" || -z "$bump_kind" ]] || die "pass either an explicit version or one bump flag"

  need_cmd awk
  need_cmd date
  need_cmd git
  need_cmd grep
  need_cmd mktemp
  need_cmd bash

  local repo_root
  repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "must be run inside a git repository"
  cd "$repo_root"

  ensure_clean_worktree

  local old_version
  old_version="$(current_version)"
  if [[ -z "$old_version" ]]; then
    old_version="0.0.0"
    printf 'No previous vX.Y.Z tag found, starting from %s\n' "$old_version"
  fi
  [[ "$old_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "current version must match x.y.z"

  if [[ -n "$bump_kind" ]]; then
    version="$(increment_version "$old_version" "$bump_kind")"
  fi

  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must match x.y.z"
  [[ "$old_version" != "$version" ]] || die "version is already $version"

  local tag_name="v$version"
  local previous_tag
  local release_date

  if ((run_push)); then
    ensure_remote_exists
  fi

  ensure_tag_absent "$tag_name"

  [[ -f "$CONTAINERFILE" ]] || die "missing $CONTAINERFILE"

  check_shell_syntax
  check_docs
  if ((run_build)); then
    build_image "$version"
  fi

  previous_tag="$(previous_release_tag)"
  release_date="$(date +%Y-%m-%d)"

  update_changelog "$version" "$release_date" "$previous_tag"

  git add "$CHANGELOG_FILE"
  git commit -m "release: $tag_name"

  git tag -a "$tag_name" -m "release: $tag_name"

  if ((run_push)); then
    git push "$REMOTE_NAME" HEAD
    git push "$REMOTE_NAME" "$tag_name"
  fi

  printf '\nReleased %s -> %s (%s)\n' "$old_version" "$version" "$tag_name"
  if ((run_push)); then
    printf 'Pushing %s started the image build and the documentation deploy.\n' "$tag_name"
  else
    printf 'Tag is local only. Push it with: git push %s %s\n' "$REMOTE_NAME" "$tag_name"
  fi
}

main "$@"
