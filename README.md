# AndroidLibXrayLite

## Build requirements
* JDK
* Android SDK
* Go
* gomobile

## Build instructions
1. `git clone [repo] && cd AndroidLibXrayLite`
2. `gomobile init`
3. `go mod tidy -v`
4. `gomobile bind -v -androidapi 21 -ldflags='-s -w' ./`

## Per-App Routing Support (Android 10+)

AndroidLibXrayLite supports per-app routing on Android 10 and above using the `ProcessFinder` interface.

### Usage

1. Implement `ProcessFinder` in your Android app (Kotlin example):

```kotlin
import android.content.Context
import android.net.ConnectivityManager
import android.os.Build
import android.system.OsConstants
import androidx.annotation.RequiresApi
import libv2ray.ProcessFinder
import java.net.InetSocketAddress

class XrayProcessFinder(context: Context) : ProcessFinder {
    private val connectivityManager =
        context.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager

    @RequiresApi(Build.VERSION_CODES.Q)
    override fun findProcessByConnection(
        network: String, srcIP: String, srcPort: Int,
        destIP: String, destPort: Int
    ): Int {
        return try {
            val protocol = if (network == "tcp") OsConstants.IPPROTO_TCP else OsConstants.IPPROTO_UDP
            connectivityManager.getConnectionOwnerUid(
                protocol,
                InetSocketAddress(srcIP, srcPort),
                InetSocketAddress(destIP, destPort)
            )
        } catch (e: Exception) {
            -1
        }
    }
}
```

2. Register before starting the core:

```kotlin
val finder = XrayProcessFinder(context)
Libv2ray.registerProcessFinder(finder)

// Start core...
coreController.startLoop(config, tunFd)
```

3. Unregister when the core is stopped:

```kotlin
Libv2ray.registerProcessFinder(null)
```

4. Configure routing rules with UIDs (obtain UIDs via `PackageManager.getPackageUid()`):

```json
{
  "routing": {
    "rules": [{
      "type": "field",
      "process": ["10123", "10156"],
      "outboundTag": "proxy"
    }]
  }
}
```

> **Note:** This feature requires Android 10 (API 29) or higher and the `android.permission.INTERNET` permission.
