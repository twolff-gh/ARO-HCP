@description('Name of the existing Kusto cluster')
param kustoName string

@description('Target Kusto database name')
param databaseName string

@description('Full resource ID of the Cosmos DB account')
param cosmosDbAccountResourceId string

@description('Name of the Cosmos DB SQL database')
param cosmosDbDatabaseName string

@description('Prefix for data connection resource names')
param dataConnectionNamePrefix string

@description('Cosmos DB containers to create data connections for')
param containers array

@description('Whether the Kusto cluster is enabled in this region')
param kustoEnabled bool

@description('Whether the Cosmos DB data connection feature is enabled')
param cosmosDataConnectionEnabled bool

resource kustoCluster 'Microsoft.Kusto/clusters@2024-04-13' existing = if (kustoEnabled) {
  name: kustoName
}

resource cosmosDataConnection 'Microsoft.Kusto/clusters/databases/dataConnections@2024-04-13' = [
  for container in containers: if (kustoEnabled && cosmosDataConnectionEnabled) {
    name: '${kustoName}/${databaseName}/${dataConnectionNamePrefix}-${toLower(container.containerName)}'
    location: resourceGroup().location
    kind: 'CosmosDb'
    properties: {
      tableName: container.tableName
      mappingRuleName: container.mappingRuleName
      managedIdentityResourceId: kustoCluster.id
      cosmosDbAccountResourceId: cosmosDbAccountResourceId
      cosmosDbDatabase: cosmosDbDatabaseName
      cosmosDbContainer: container.containerName
    }
  }
]
