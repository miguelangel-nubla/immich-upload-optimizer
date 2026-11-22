#!/usr/bin/env bash
set -euxo pipefail

if [[ $# -lt 3 ]]; then
  echo "Usage: $0 <folder> <name> <extension>"
  exit 1
fi

ORIGINAL_FILE_NO_EXTENSION="${1}/${2}"
ORIGINAL_FILE="${ORIGINAL_FILE_NO_EXTENSION}.${3}"

# The value is read as text and compared as text. Inside [[ ]], -gt would
# evaluate it as an arithmetic expression, and it comes from the uploaded file.
MOTION_PHOTO="$(exiftool -s3 -MotionPhoto "$ORIGINAL_FILE" || true)"
if [[ "$MOTION_PHOTO" == "1" ]]; then
  # TODO: Implement. https://github.com/miguelangel-nubla/immich-upload-optimizer/issues/12#issuecomment-2642944560
  echo "Original file is a HEIC Motion Photo, will not optimize"
  exit 0
else
  vips copy "${ORIGINAL_FILE}" "${ORIGINAL_FILE_NO_EXTENSION}-new.jxl[Q=75]"
  rm "${ORIGINAL_FILE}"
fi