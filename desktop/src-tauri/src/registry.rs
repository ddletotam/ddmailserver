use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;

use crate::provider::MailProvider;

/// Maps account_id → provider instance.
///
/// Registered as Tauri managed state. Providers are created when an
/// account is activated and looked up by subsequent commands.
pub struct ProviderRegistry {
    providers: RwLock<HashMap<String, Arc<dyn MailProvider>>>,
}

impl ProviderRegistry {
    pub fn new() -> Self {
        Self {
            providers: RwLock::new(HashMap::new()),
        }
    }

    /// Register (or replace) a provider for the given account.
    pub async fn register(&self, account_id: &str, provider: Arc<dyn MailProvider>) {
        self.providers
            .write()
            .await
            .insert(account_id.to_string(), provider);
    }

    /// Look up the provider for an account.
    pub async fn get(&self, account_id: &str) -> Option<Arc<dyn MailProvider>> {
        self.providers.read().await.get(account_id).cloned()
    }

    /// Remove the provider when an account is deactivated.
    pub async fn remove(&self, account_id: &str) {
        self.providers.write().await.remove(account_id);
    }
}
