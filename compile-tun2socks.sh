#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename ${__file} .sh)"

if [[ ! -d $NDK_HOME ]]; then
	echo "Android NDK: NDK_HOME not found. please set env \$NDK_HOME"
	exit 1
fi

TMPDIR=$(mktemp -d)
clear_tmp () {
  rm -rf $TMPDIR
}

trap 'echo -e "Aborted, error $? in command: $BASH_COMMAND"; trap ERR; clear_tmp; exit 1' ERR INT
install -m644 $__dir/tun2socks.mk $TMPDIR/

abi_filter="all"
if [ "$#" -eq 1 ]; then
  case $1 in
    arm64-v8a)
      abi_filter="arm64-v8a"
      ;;
    armeabi-v7a)
      abi_filter="armeabi-v7a"
      ;;
    x86_64)
      abi_filter="x86_64"
      ;;
    x86)
      abi_filter="x86"
      ;;
    *)
      echo "Invalid ABI specified: $1"
      echo "Valid options are: arm64-v8a, armeabi-v7a, x86_64, x86"
      exit 1
      ;;
  esac
fi

pushd $TMPDIR
ln -s $__dir/badvpn badvpn
ln -s $__dir/libancillary libancillary
$NDK_HOME/ndk-build \
	NDK_PROJECT_PATH=. \
	APP_BUILD_SCRIPT=./tun2socks.mk \
	APP_ABI=$abi_filter \
	APP_PLATFORM=android-19 \
	NDK_LIBS_OUT=$TMPDIR/libs \
	NDK_OUT=$TMPDIR/tmp \
	APP_SHORT_COMMANDS=false LOCAL_SHORT_COMMANDS=false -B -j4 \
        LOCAL_LDFLAGS=-Wl,--build-id=none

tar cvfz $__dir/libtun2socks.so.tgz libs
popd

rm -rf $TMPDIR
