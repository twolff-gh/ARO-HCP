@description('Name of the existing Kusto cluster')
param kustoName string

@description('Full resource ID of the Cosmos DB account')
param cosmosDbAccountResourceId string

@description('Whether the Cosmos DB account has public network access disabled')
param cosmosDbPrivate bool

@description('Whether the Kusto cluster is enabled in this region')
param kustoEnabled bool

@description('Whether the Cosmos DB data connection feature is enabled')
param cosmosDataConnectionEnabled bool

resource kustoCluster 'Microsoft.Kusto/clusters@2024-04-13' existing = if (kustoEnabled) {
  name: kustoName
}

resource cosmosManagedEndpoint 'Microsoft.Kusto/clusters/managedPrivateEndpoints@2024-04-13' = if (kustoEnabled && cosmosDataConnectionEnabled && cosmosDbPrivate) {
  parent: kustoCluster
  name: 'cosmos-sql'
  properties: {
    groupId: 'Sql'
    privateLinkResourceId: cosmosDbAccountResourceId
    requestMessage: 'Kusto Cosmos DB data connection'
  }
}
