@description('Toggle if kusto instance is expected to exist')
param kustoEnabled bool

@description('Toggle if eventhub instance is expected to exist')
param eventhubEnabled bool

@description('Event Hub namespace for AKS audit logs')
param auditLogsEventHubNamespaceName string

@description('Name of the event hub authorization rule for AKS audit logs')
param auditLogsEventHubAuthRuleName string

resource auditLogsEventHubNamespace 'Microsoft.EventHub/namespaces@2024-01-01' existing = if (kustoEnabled && eventhubEnabled) {
  name: auditLogsEventHubNamespaceName

  resource diagnosticSettingsAuthRule 'authorizationRules@2024-01-01' existing = {
    name: auditLogsEventHubAuthRuleName
  }
}

output auditLogsEventHubAuthRuleId string = kustoEnabled && eventhubEnabled
  ? auditLogsEventHubNamespace::diagnosticSettingsAuthRule.id
  : ''
