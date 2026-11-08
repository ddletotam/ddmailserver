package ru.letotam.ddmail

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.unit.dp

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            MaterialTheme {
                Scaffold { inner ->
                    Probe(Modifier.padding(inner))
                }
            }
        }
    }
}

/** Каркас: показывает размер окна — по нему на Mate XT сверяем классы
 *  ширины трёх состояний (контракт §6), прежде чем строить раскладку. */
@Composable
private fun Probe(modifier: Modifier = Modifier) {
    val cfg = LocalConfiguration.current
    Column(
        modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Text("ddmail ${BuildConfig.VERSION_NAME}", style = MaterialTheme.typography.headlineSmall)
        Text("Окно: ${cfg.screenWidthDp} × ${cfg.screenHeightDp} dp")
        Text("Плотность: ${cfg.densityDpi} dpi")
    }
}
