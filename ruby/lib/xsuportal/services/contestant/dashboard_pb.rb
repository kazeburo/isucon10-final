# Generated by the protocol buffer compiler.  DO NOT EDIT!
# source: xsuportal/services/contestant/dashboard.proto

require 'google/protobuf'

require 'xsuportal/resources/leaderboard_pb'
require 'xsuportal/resources/benchmark_job_pb'
Google::Protobuf::DescriptorPool.generated_pool.build do
  add_file("xsuportal/services/contestant/dashboard.proto", :syntax => :proto3) do
    add_message "xsuportal.proto.services.contestant.DashboardRequest" do
    end
    add_message "xsuportal.proto.services.contestant.DashboardResponse" do
      optional :leaderboard, :message, 1, "xsuportal.proto.resources.Leaderboard"
    end
  end
end

module Xsuportal
  module Proto
    module Services
      module Contestant
        DashboardRequest = ::Google::Protobuf::DescriptorPool.generated_pool.lookup("xsuportal.proto.services.contestant.DashboardRequest").msgclass
        DashboardResponse = ::Google::Protobuf::DescriptorPool.generated_pool.lookup("xsuportal.proto.services.contestant.DashboardResponse").msgclass
      end
    end
  end
end
