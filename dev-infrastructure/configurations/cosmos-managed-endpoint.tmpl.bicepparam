using '../templates/cosmos-managed-endpoint.bicep'

param kustoName = '{{ .kusto.kustoName }}'
param cosmosDbAccountResourceId = '/subscriptions/{{ .svc.subscription.id }}/resourceGroups/{{ .svc.rg }}/providers/Microsoft.DocumentDB/databaseAccounts/{{ .frontend.cosmosDB.name }}'
param cosmosDbPrivate = {{ .frontend.cosmosDB.private }}
param kustoEnabled = {{ .arobit.kusto.enabled }}
param cosmosDataConnectionEnabled = {{ .cosmosKustoDataConnection.enabled }}
