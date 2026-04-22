using '../templates/cosmos-rbac-kusto.bicep'

param cosmosDbName = '{{ .frontend.cosmosDB.name }}'
param kustoPrincipalId = '__kustoPrincipalId__'
param cosmosDataConnectionEnabled = {{ .cosmosKustoDataConnection.enabled }}
