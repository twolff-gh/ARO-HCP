@description('Name of the Cosmos DB account to grant access to')
param cosmosDbName string

@description('Principal ID of the Kusto cluster managed identity')
param kustoPrincipalId string

@description('Whether the Cosmos DB data connection feature is enabled')
param cosmosDataConnectionEnabled bool

resource cosmosDbAccount 'Microsoft.DocumentDB/databaseAccounts@2023-11-15' existing = {
  name: cosmosDbName
}

// Cosmos DB Built-in Data Reader (data-plane read access via change feed)
var cosmosDbDataReaderRole = '00000000-0000-0000-0000-000000000001'

resource sqlRoleAssignment 'Microsoft.DocumentDB/databaseAccounts/sqlRoleAssignments@2021-04-15' = if (cosmosDataConnectionEnabled && kustoPrincipalId != '') {
  name: guid(cosmosDbDataReaderRole, kustoPrincipalId, cosmosDbAccount.id)
  parent: cosmosDbAccount
  properties: {
    roleDefinitionId: '/${subscription().id}/resourceGroups/${resourceGroup().name}/providers/Microsoft.DocumentDB/databaseAccounts/${cosmosDbAccount.name}/sqlRoleDefinitions/${cosmosDbDataReaderRole}'
    principalId: kustoPrincipalId
    scope: cosmosDbAccount.id
  }
}

// Cosmos DB Account Reader Role (control-plane metadata access for data connection)
var cosmosDbAccountReaderRole = 'fbdf93bf-df7d-467e-a4d2-9458aa1360c8'

resource rbacRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (cosmosDataConnectionEnabled && kustoPrincipalId != '') {
  name: guid(cosmosDbAccountReaderRole, kustoPrincipalId, cosmosDbAccount.id)
  scope: cosmosDbAccount
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', cosmosDbAccountReaderRole)
    principalId: kustoPrincipalId
    principalType: 'ServicePrincipal'
  }
}
