using '../templates/cosmos-kusto-data-connection.bicep'

param kustoName = '{{ .kusto.kustoName }}'
param databaseName = '{{ .kusto.serviceLogsDatabase }}'
param cosmosDbAccountResourceId = '/subscriptions/{{ .svc.subscription.id }}/resourceGroups/{{ .svc.rg }}/providers/Microsoft.DocumentDB/databaseAccounts/{{ .frontend.cosmosDB.name }}'
param cosmosDbDatabaseName = '{{ .frontend.cosmosDB.name }}'
param dataConnectionNamePrefix = '{{ .cosmosKustoDataConnection.dataConnectionNamePrefix }}'
param containers = [
  {
    containerName: 'Billing'
    tableName: 'cosmosBilling'
    mappingRuleName: 'CosmosBillingMapping'
  }
  {
    containerName: 'Resources'
    tableName: 'cosmosResources'
    mappingRuleName: 'CosmosResourcesMapping'
  }
]
param kustoEnabled = {{ .arobit.kusto.enabled }}
param cosmosDataConnectionEnabled = {{ .cosmosKustoDataConnection.enabled }}
