// Build, test, scan and roll out shop-api. Stages that need a fleet read it from
// FLEET_FILE (see examples/fleet.ssh.json). This file has not been run on a
// Jenkins server; every command in it was run by hand or in the test suite.
pipeline {
  agent any

  options {
    timestamps()
    timeout(time: 60, unit: 'MINUTES')
    disableConcurrentBuilds()
    buildDiscarder(logRotator(numToKeepStr: '30', artifactNumToKeepStr: '10'))
  }

  parameters {
    string(name: 'FLEET_FILE', defaultValue: 'examples/fleet.ssh.json', description: 'Fleet definition for the target environment')
    string(name: 'BATCH_PERCENT', defaultValue: '5', description: 'Percent of hosts that may be out of rotation at once')
    string(name: 'CANARY_HOSTS', defaultValue: '2', description: 'Hosts updated first and judged on live numbers; 0 for a plain rolling update')
    string(name: 'STAGING_URL', defaultValue: 'https://staging.example.com/', description: 'Checked for security headers after the staging deploy')
    booleanParam(name: 'DEPLOY', defaultValue: false, description: 'Roll the build out; otherwise stop after the dry run')
  }

  environment {
    APP_VERSION = "${env.BUILD_NUMBER}-${env.GIT_COMMIT ? env.GIT_COMMIT.take(7) : 'local'}"
    SPLUNK_HEC_URL = credentials('splunk-hec-url')
    SPLUNK_HEC_TOKEN = credentials('splunk-hec-token')
    GOFLAGS = '-mod=readonly'
  }

  stages {
    stage('Build and test') {
      parallel {
        stage('Java (Gradle)') {
          steps {
            sh 'gradle -p java-app --no-daemon -PappVersion=$APP_VERSION build'
          }
          post {
            always { junit allowEmptyResults: true, testResults: 'java-app/build/test-results/test/*.xml' }
          }
        }
        stage('Go') {
          steps {
            sh '''
              gofmt -l . | tee gofmt.out
              test ! -s gofmt.out
              go vet ./...
              go test -race -count=1 ./...
            '''
          }
        }
      }
    }

    stage('Security gate') {
      steps {
        sh '''
          trivy fs --scanners vuln --format json --output trivy.json .
          go run ./cmd/secgate trivy -policy policy/security.json trivy.json
          govulncheck ./...
        '''
      }
    }

    stage('Package') {
      steps {
        archiveArtifacts artifacts: 'java-app/build/libs/*.jar', fingerprint: true
      }
    }

    stage('Plan the rollout') {
      steps {
        sh 'go build -o pushbutton ./cmd/pushbutton'
        sh './pushbutton deploy -fleet "$FLEET_FILE" -version "$APP_VERSION" -batch "$BATCH_PERCENT" -dry-run'
      }
    }

    stage('Roll out') {
      when { expression { return params.DEPLOY } }
      steps {
        input message: "Roll ${env.APP_VERSION} out to ${params.FLEET_FILE}?", ok: 'Deploy'
        sh './pushbutton deploy -fleet "$FLEET_FILE" -version "$APP_VERSION" -batch "$BATCH_PERCENT" -canary "$CANARY_HOSTS" -report rollout.json'
      }
      post {
        always { archiveArtifacts artifacts: 'rollout.json', allowEmptyArchive: true }
      }
    }

    stage('Check what is live') {
      when { expression { return params.DEPLOY } }
      steps {
        sh 'go run ./cmd/secgate headers "$STAGING_URL"'
      }
    }
  }

  post {
    // pushbutton exits 1 after a rollback and 3 when the rollback itself failed.
    // Jenkins marks the build failed for either; only the second needs a person right now.
    failure {
      echo 'The build failed. If rollout.json says rollback_failed, a host may be out of rotation: page the on-call engineer.'
    }
  }
}
